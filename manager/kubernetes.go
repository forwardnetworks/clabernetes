package manager

import (
	clabernetesapisv1alpha1 "github.com/srl-labs/clabernetes/apis/v1alpha1"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrlruntime "sigs.k8s.io/controller-runtime"
	ctrlruntimecache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlruntimemetricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func newManager(scheme *apimachineryruntime.Scheme, appName string) (ctrlruntime.Manager, error) {
	return ctrlruntime.NewManager(
		ctrlruntime.GetConfigOrDie(),
		ctrlruntime.Options{
			Logger: klog.NewKlogr(),
			Scheme: scheme,
			Metrics: ctrlruntimemetricsserver.Options{
				BindAddress: "0",
			},
			LeaderElection: false,
			NewCache: func(
				config *rest.Config,
				opts ctrlruntimecache.Options,
			) (ctrlruntimecache.Cache, error) {
				// NOTE: upstream clabernetes filters the default cache by "clabernetes/app" to reduce
				// memory. In Skyforge, we routinely create namespace-scoped objects (ServiceAccounts,
				// ConfigMaps, etc.) that may pre-exist or be created without that label (e.g., legacy
				// resources, or objects created before clabernetes is installed).
				//
				// If we keep the label-filtered cache, controller-runtime Get() calls can return
				// NotFound even when the object exists, causing reconcile loops to stall (e.g.:
				// "ServiceAccount clabernetes-launcher-service-account not found").
				//
				// For Skyforge, correctness > micro-optimizations; keep the default cache unfiltered.
				_ = appName

				opts.ByObject = map[ctrlruntimeclient.Object]ctrlruntimecache.ByObject{
					// obviously we need to cache all "our" topology objects, so do that
					&clabernetesapisv1alpha1.Topology{}: {
						Namespaces: map[string]ctrlruntimecache.Config{
							ctrlruntimecache.AllNamespaces: {
								LabelSelector: labels.Everything(),
							},
						},
					},
					// we need to cache all our image request crs too of course
					&clabernetesapisv1alpha1.ImageRequest{}: {
						Namespaces: map[string]ctrlruntimecache.Config{
							ctrlruntimecache.AllNamespaces: {
								LabelSelector: labels.Everything(),
							},
						},
					},
					// watch our config "singleton" too; while this is sorta/basically a "cluster"
					// CR -- we dont want to have to force users to have cluster wide perms, *and*
					// we want to be able to set an owner ref to the manager deployment, so the
					// config *is* namespaced, so... watch all the namespaces for the config...
					&clabernetesapisv1alpha1.Config{}: {
						Namespaces: map[string]ctrlruntimecache.Config{
							ctrlruntimecache.AllNamespaces: {
								LabelSelector: labels.Everything(),
							},
						},
					},
					// our tunnel "connectivity" cr
					&clabernetesapisv1alpha1.Connectivity{}: {
						Namespaces: map[string]ctrlruntimecache.Config{
							ctrlruntimecache.AllNamespaces: {
								LabelSelector: labels.Everything(),
							},
						},
					},
				}

				return ctrlruntimecache.New(config, opts)
			},
		},
	)
}
