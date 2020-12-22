package controller

import (
	"reflect"
	"testing"

	"github.com/ghodss/yaml"
	kubelessApi "github.com/kubeless/kubeless/pkg/apis/kubeless/v1beta1"
	"github.com/kubeless/kubeless/pkg/langruntime"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/autoscaling/v2beta1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func findAction(fake *fake.Clientset, verb, resource string) ktesting.Action {
	for _, a := range fake.Actions() {
		if a.Matches(verb, resource) {
			return a
		}
	}
	return nil
}

func hasAction(fake *fake.Clientset, verb, resource string) bool {
	return findAction(fake, verb, resource) != nil
}

func TestDeleteK8sResources(t *testing.T) {
	myNsFoo := metav1.ObjectMeta{
		Namespace: "myns",
		Name:      "foo",
	}

	deploy := appsv1.Deployment{
		ObjectMeta: myNsFoo,
	}

	svc := v1.Service{
		ObjectMeta: myNsFoo,
	}

	cm := v1.ConfigMap{
		ObjectMeta: myNsFoo,
	}

	hpa := v2beta1.HorizontalPodAutoscaler{
		ObjectMeta: myNsFoo,
	}

	clientset := fake.NewSimpleClientset(&deploy, &svc, &cm, &hpa)

	controller := FunctionController{
		clientset: clientset,
	}
	if err := controller.deleteK8sResources("myns", "foo"); err != nil {
		t.Fatalf("Deleting resources returned err: %v", err)
	}

	t.Log("Actions:", clientset.Actions())

	for _, kind := range []string{"services", "configmaps", "deployments", "horizontalpodautoscalers"} {
		a := findAction(clientset, "delete", kind)
		if a == nil {
			t.Errorf("failed to delete %s", kind)
		} else if ns := a.GetNamespace(); ns != "myns" {
			t.Errorf("deleted %s from wrong namespace (%s)", kind, ns)
		} else if n := a.(ktesting.DeleteAction).GetName(); n != "foo" {
			t.Errorf("deleted %s with wrong name (%s)", kind, n)
		}
	}

	// Similar with only svc remaining
	clientset = fake.NewSimpleClientset(&svc)
	controller = FunctionController{
		clientset: clientset,
	}

	if err := controller.deleteK8sResources("myns", "foo"); err != nil {
		t.Fatalf("Deleting partial resources returned err: %v", err)
	}

	t.Log("Actions:", clientset.Actions())

	if !hasAction(clientset, "delete", "services") {
		t.Errorf("failed to delete service")
	}

	clientset = fake.NewSimpleClientset(&deploy, &svc, &cm)
	controller = FunctionController{
		clientset: clientset,
	}

	if err := controller.deleteK8sResources("myns", "foo"); err != nil {
		t.Fatalf("Deleting resources returned err: %v", err)
	}

	t.Log("Actions:", clientset.Actions())

	for _, kind := range []string{"services", "configmaps", "deployments"} {
		a := findAction(clientset, "delete", kind)
		if a == nil {
			t.Errorf("failed to delete %s", kind)
		} else if ns := a.GetNamespace(); ns != "myns" {
			t.Errorf("deleted %s from wrong namespace (%s)", kind, ns)
		}
	}
}

func TestEnsureK8sResourcesWithDeploymentDefinitionFromConfigMap(t *testing.T) {
	funcObj := testFunc()
	deploymentConfigData := `{
		"metadata": {
			"annotations": {
				"foo-from-deploy-cm": "bar-from-deploy-cm",
				"xyz": "valuefromcm"
			}
		},
		"spec": {
			"replicas": 2,
			"template": {
				"metadata": {
					"annotations": {
					"podannotation-from-func-crd": "value-from-container"
					}
				}
			}
		}
	}`

	clientset := fake.NewSimpleClientset()
	controller := testController(clientset, funcObj.Namespace, map[string]string{
		"deployment":     deploymentConfigData,
		"runtime-images": testRuntimeImages(),
	})

	if err := controller.ensureK8sResources(funcObj); err != nil {
		t.Fatalf("Creating/Updating resources returned err: %v", err)
	}
	dpm, _ := clientset.AppsV1().Deployments(funcObj.Namespace).Get(funcObj.Name, metav1.GetOptions{})
	expectedAnnotations := map[string]string{
		"bar":                "foo",
		"foo-from-deploy-cm": "bar-from-deploy-cm",
		"xyz":                "valuefromfunc",
	}
	for i := range expectedAnnotations {
		if dpm.ObjectMeta.Annotations[i] != expectedAnnotations[i] {
			t.Errorf("Expecting annotation %s but received %s", expectedAnnotations[i], dpm.ObjectMeta.Annotations[i])
		}
	}
	if *dpm.Spec.Replicas != 10 {
		t.Fatalf("Expecting replicas as 10 but received : %d", *dpm.Spec.Replicas)
	}
	expectedPodAnnotations := map[string]string{
		"bar":                         "foo",
		"foo-from-deploy-cm":          "bar-from-deploy-cm",
		"xyz":                         "valuefromfunc",
		"podannotation-from-func-crd": "value-from-container",
	}
	for i := range expectedPodAnnotations {
		if dpm.Spec.Template.Annotations[i] != expectedPodAnnotations[i] {
			t.Fatalf("Expecting annotation %s but received %s", expectedPodAnnotations[i], dpm.ObjectMeta.Annotations[i])
		}
	}
}

func TestEnsureK8sResourcesWithDeploymentDefinitionFromConfigMapUnknownKey(t *testing.T) {
	funcObj := testFunc()
	deploymentConfigData := `{
		"spec": {
			"template": {
				"spec": {
					"unknown": "property"
				}
			}
		}
	}`
	controller := testController(fake.NewSimpleClientset(), funcObj.Namespace, map[string]string{
		"deployment":     deploymentConfigData,
		"runtime-images": testRuntimeImages(),
	})

	if err := controller.ensureK8sResources(funcObj); err == nil {
		t.Fatalf("Unknown key in ConfigMap Deployment definition does not fail")
	}
}

func TestEnsureK8sResourcesWithLivenessProbeFromConfigMap(t *testing.T) {
	funcObj := testFunc()
	runtimeImages := `[
		{
			"ID": "ruby",
			"depName": "Gemfile",
			"fileNameSuffix": ".rb",
			"versions": [
				{
					"name": "ruby24",
					"version": "2.4",
					"initImage": "bitnami/ruby:2.4",
					"imagePullSecrets":[]
				}
			],
			"livenessProbeInfo":{
				"exec": {
					"command": [
						"curl",
						"-f",
						"http://localhost:8080/healthz"
					],
				},
				"initialDelaySeconds": 5,
				"periodSeconds": 10
			}
		}
	]`

	clientset := fake.NewSimpleClientset()
	controller := testController(clientset, funcObj.Namespace, map[string]string{
		"runtime-images": runtimeImages,
	})

	if err := controller.ensureK8sResources(funcObj); err != nil {
		t.Fatalf("Creating/Updating resources returned err: %v", err)
	}
	dpm, _ := clientset.AppsV1().Deployments(funcObj.Namespace).Get(funcObj.Name, metav1.GetOptions{})
	expectedLivenessProbe := &v1.Probe{
		InitialDelaySeconds: int32(5),
		PeriodSeconds:       int32(10),
		Handler: v1.Handler{
			Exec: &v1.ExecAction{
				Command: []string{"curl", "-f", "http://localhost:8080/healthz"},
			},
		},
	}

	if !reflect.DeepEqual(dpm.Spec.Template.Spec.Containers[0].LivenessProbe, expectedLivenessProbe) {
		t.Fatalf("LivenessProbe found is '%v', although expected was '%v'", dpm.Spec.Template.Spec.Containers[0].LivenessProbe, expectedLivenessProbe)
	}

}

func testFunc() *kubelessApi.Function {
	var replicas int32
	replicas = 10
	funcAnno := map[string]string{
		"bar": "foo",
		"xyz": "valuefromfunc",
	}
	return &kubelessApi.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: "default",
			Labels:    map[string]string{"foo": "bar"},
			UID:       "foo-uid",
		},
		Spec: kubelessApi.FunctionSpec{
			Function: "function",
			Deps:     "deps",
			Handler:  "foo.bar",
			Runtime:  "ruby2.4",
			Deployment: appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: funcAnno,
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &replicas,
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Annotations: funcAnno,
						},
						Spec: v1.PodSpec{
							Containers: []v1.Container{
								{
									Env: []v1.EnvVar{
										{
											Name:  "foo",
											Value: "bar",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func testRuntimeImages() string {
	runtimeImages := []langruntime.RuntimeInfo{{
		ID:             "ruby",
		DepName:        "Gemfile",
		FileNameSuffix: ".rb",
		Versions: []langruntime.RuntimeVersion{
			{
				Name:    "ruby24",
				Version: "2.4",
				Images: []langruntime.Image{
					{Phase: "runtime", Image: "bitnami/ruby:2.4"},
				},
				ImagePullSecrets: []langruntime.ImageSecret{},
			},
		},
	}}

	out, err := yaml.Marshal(runtimeImages)
	if err != nil {
		logrus.Fatal("Canot Marshall runtimeimage")
	}
	return string(out)
}

func testController(clientset kubernetes.Interface, namespace string, configData map[string]string) *FunctionController {
	kubelessConfigMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeless-config",
		},
		Data: configData,
	}
	_, err := clientset.CoreV1().ConfigMaps(namespace).Create(kubelessConfigMap)
	if err != nil {
		logrus.Fatal("Unable to create configmap")
	}

	config, err := clientset.CoreV1().ConfigMaps(namespace).Get("kubeless-config", metav1.GetOptions{})
	if err != nil {
		logrus.Fatal("Unable to read the configmap")
	}
	var lr = langruntime.New(config)
	lr.ReadConfigMap()

	return &FunctionController{
		logger:      logrus.WithField("pkg", "controller"),
		clientset:   clientset,
		langRuntime: lr,
		config:      config,
	}
}
