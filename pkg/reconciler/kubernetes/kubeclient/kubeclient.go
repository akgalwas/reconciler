// solution from https://github.com/billiford/go-clouddriver/blob/master/pkg/kubernetes/client.go

package kubeclient

import (
	"context"
	"encoding/base64"
	"strings"

	k8s "github.com/kyma-incubator/reconciler/pkg/reconciler/kubernetes"
	"github.com/pkg/errors"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/kubectl/pkg/util"
)

type KubeClient struct {
	Base64KubeConfig string
	DynamicClient    dynamic.Interface
	Config           *rest.Config
	Mapper           *restmapper.DeferredDiscoveryRESTMapper
}

func NewKubeClient(kubeconfig string) (*KubeClient, error) {
	base64kubeConfig := base64.StdEncoding.EncodeToString([]byte(kubeconfig))
	client := KubeClient{
		Base64KubeConfig: base64kubeConfig,
	}
	dynamicClient, err := client.getDynamicClient()
	if err != nil {
		return nil, err
	}
	client.DynamicClient = dynamicClient

	restClient, err := client.getDiscoveryMapper()
	if err != nil {
		return nil, err
	}
	client.Mapper = restClient

	config, err := client.buildRestConfig()
	if err != nil {
		return nil, err
	}
	client.Config = config

	return &KubeClient{
		Base64KubeConfig: base64kubeConfig,
		DynamicClient:    dynamicClient,
		Config:           config,
		Mapper:           restClient,
	}, nil
}

func (kube *KubeClient) Apply(u *unstructured.Unstructured) (*k8s.Resource, error) {
	return kube.ApplyWithNamespaceOverride(u, "")
}

// Apply a given manifest with an optional namespace to override.
// If no namespace is set on the manifest and no namespace override is passed in then we set the namespace to 'default'.
// If namespaceOverride is empty it will NOT override the namespace set on the manifest.
// We only override the namespace if the manifest is NOT cluster scoped (i.e. a ClusterRole) and namespaceOverride is NOT an
// empty string.
func (kube *KubeClient) ApplyWithNamespaceOverride(u *unstructured.Unstructured, namespaceOverride string) (*k8s.Resource, error) {
	metadata := &k8s.Resource{}
	gvk := u.GroupVersionKind()

	restMapping, err := kube.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return metadata, err
	}

	gv := gvk.GroupVersion()
	kube.Config.GroupVersion = &gv

	restClient, err := newRestClient(*kube.Config, gv)
	if err != nil {
		return metadata, err
	}

	helper := resource.NewHelper(restClient, restMapping)

	if namespaceOverride == "" {
		SetDefaultNamespaceIfScopedAndNoneSet(u, helper)
	} else {
		SetNamespaceIfScoped(namespaceOverride, u, helper)
	}

	info := &resource.Info{
		Client:          restClient,
		Mapping:         restMapping,
		Namespace:       u.GetNamespace(),
		Name:            u.GetName(),
		Source:          "",
		Object:          u,
		ResourceVersion: restMapping.Resource.Version,
	}

	patcher := newPatcher(info, helper)

	// Get the modified configuration of the object. Embed the result
	// as an annotation in the modified configuration, so that it will appear
	// in the patch sent to the server.
	modified, err := util.GetModifiedConfiguration(info.Object, true, unstructured.UnstructuredJSONScheme)
	if err != nil {
		return metadata, err
	}

	if err := info.Get(); err != nil {
		if !k8serrors.IsNotFound(err) {
			return metadata, err
		}

		// Create the resource if it doesn't exist
		// First, update the annotation used by kubectl kubeClient
		if err := util.CreateApplyAnnotation(info.Object, unstructured.UnstructuredJSONScheme); err != nil {
			return metadata, err
		}

		// Then create the resource and skip the three-way merge
		obj, err := helper.Create(info.Namespace, true, info.Object)
		if err != nil {
			return metadata, err
		}

		_ = info.Refresh(obj, true)
	}

	_, patchedObject, err := patcher.Patch(info.Object, modified, info.Namespace, info.Name)
	if err != nil {
		return metadata, err
	}

	_ = info.Refresh(patchedObject, true)

	metadata.Name = u.GetName()
	metadata.Namespace = u.GetNamespace()
	metadata.Kind = gvk.Kind

	return metadata, nil
}

func (kube *KubeClient) GetClientSet() (*kubernetes.Clientset, error) {
	restConfig, err := kube.buildRestConfig()
	if err != nil {
		return nil, errors.Wrap(err, "build restConfig failed")
	}
	return kubernetes.NewForConfig(restConfig)
}

func (kube *KubeClient) DeleteResourceByKindAndNameAndNamespace(kind, name, namespace string, do metav1.DeleteOptions) (*k8s.Resource, error) {
	gvk, err := kube.Mapper.KindFor(schema.GroupVersionResource{
		Resource: kind,
	})
	if err != nil {
		return nil, err
	}

	var isNamespaceResource = strings.ToLower(gvk.Kind) == "namespace"

	if !isNamespaceResource && namespace == "" { //set qualified namespace (except resource is of kind 'namespace')
		namespace = "default"
	}

	restMapping, err := kube.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	restClient, err := newRestClient(*kube.Config, gvk.GroupVersion())
	if err != nil {
		return nil, err
	}

	helper := resource.NewHelper(restClient, restMapping)

	if helper.NamespaceScoped {
		err = kube.DynamicClient.
			Resource(restMapping.Resource).
			Namespace(namespace).
			Delete(context.TODO(), name, do)
	} else {
		err = kube.DynamicClient.
			Resource(restMapping.Resource).
			Delete(context.TODO(), name, do)
	}

	//return deleted resource
	if isNamespaceResource {
		namespace = "" //namespace resources have always an empty namespace field
	}
	return &k8s.Resource{
		Kind:      kind,
		Name:      name,
		Namespace: namespace,
	}, err
}

// Get a manifest by resource/kind (example: 'pods' or 'pod'),
// name (example: 'my-pod'), and namespace (example: 'my-namespace').
func (kube *KubeClient) Get(kind, name, namespace string) (*unstructured.Unstructured, error) {
	gvk, err := kube.Mapper.KindFor(schema.GroupVersionResource{Resource: kind})
	if err != nil {
		return nil, err
	}

	restMapping, err := kube.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}

	restClient, err := newRestClient(*kube.Config, gvk.GroupVersion())
	if err != nil {
		return nil, err
	}

	var u *unstructured.Unstructured

	helper := resource.NewHelper(restClient, restMapping)
	if helper.NamespaceScoped {
		u, err = kube.DynamicClient.
			Resource(restMapping.Resource).
			Namespace(namespace).
			Get(context.TODO(), name, metav1.GetOptions{})
	} else {
		u, err = kube.DynamicClient.
			Resource(restMapping.Resource).
			Get(context.TODO(), name, metav1.GetOptions{})
	}

	return u, err
}

func (kube *KubeClient) GVRForKind(kind string) (schema.GroupVersionResource, error) {
	return kube.Mapper.ResourceFor(schema.GroupVersionResource{Resource: kind})
}

// ListResource lists all resources by their kind or resource (e.g. "replicaset" or "replicasets").
func (kube *KubeClient) ListResource(resource string, lo metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	gvr, err := kube.GVRForKind(resource)
	if err != nil {
		return nil, err
	}

	return kube.DynamicClient.Resource(gvr).List(context.TODO(), lo)
}

func newRestClient(restConfig rest.Config, gv schema.GroupVersion) (rest.Interface, error) {
	restConfig.ContentConfig = resource.UnstructuredPlusDefaultContentConfig()
	restConfig.GroupVersion = &gv

	if len(gv.Group) == 0 {
		restConfig.APIPath = "/api"
	} else {
		restConfig.APIPath = "/apis"
	}

	return rest.RESTClientFor(&restConfig)
}

func (kube *KubeClient) getDynamicClient() (dynamic.Interface, error) {
	restConfig, err := kube.buildRestConfig()
	if err != nil {
		return nil, errors.Wrap(err, "build restConfig failed")
	}

	return dynamic.NewForConfig(restConfig)
}

func (kube *KubeClient) getDiscoveryMapper() (*restmapper.DeferredDiscoveryRESTMapper, error) {
	restConfig, err := kube.buildRestConfig()
	if err != nil {
		return nil, errors.Wrap(err, "build restConfig failed")
	}

	// Prepare a RESTMapper to find GVR
	dc, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, errors.Wrap(err, "new dc failed")
	}

	discoveryMapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
	return discoveryMapper, nil
}

func (kube *KubeClient) buildRestConfig() (resetConfig *rest.Config, err error) {
	kubeConfig, err := base64.StdEncoding.DecodeString(kube.Base64KubeConfig)
	if err != nil {
		return nil, err
	}

	conf, err := clientcmd.BuildConfigFromKubeconfigGetter("", func() (config *clientcmdapi.Config, e error) {
		return clientcmd.Load(kubeConfig)
	})

	if err != nil {
		return nil, err
	}
	return conf, nil
}