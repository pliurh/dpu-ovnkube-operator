package main

import (
	"context"
	"flag"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"

	syncer "github.com/openshift/dpu-ovnkube-operator/pkg/ovnkube-syncer"
)

type leaderConfig struct {
	LeaseDuration int64
	RenewDeadline int64
	RetryPeriod   int64
}

const (
	leadershipConfigEnvPrefix = "leadership"
	defaultLeaseDuration      = 100 * time.Second
	defaultRenewDeadline      = 50 * time.Second
	defaultRetryPeriod        = 20 * time.Second
)

func main() {
	var config *rest.Config
	var err error

	klog.InitFlags(nil)
	flag.Parse()

	klog.Info("Starting the ovnkube syncer")

	namespace := os.Getenv("NAMESPACE")
	name := os.Getenv("POD_NAME")

	kubeconfig := os.Getenv("KUBECONFIG")
	config, err = buildConfig(kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	kubeClient := kubernetes.NewForConfigOrDie(config)

	tenantKubeconfig := os.Getenv("TENANT_KUBECONFIG")
	cfg, err := clientcmd.BuildConfigFromFlags("", tenantKubeconfig)
	if err != nil {
		klog.Fatal(err)
	}

	tenantNamespace := os.Getenv("TENANT_NAMESPACE")

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      "config-daemon-draining-lock",
			Namespace: namespace,
		},
		Client: kubeClient.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: name,
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// set up signals so we handle the first shutdown signal gracefully
	stopCh := signals.SetupSignalHandler().Done()
	go func() {
		<-stopCh
		klog.Info("Received termination, signaling shutdown")
		cancel()
	}()

	becameLeader := func(context.Context) {
		klog.Info("Creating the datastore syncer")

		ovnSyncer, err := syncer.New(syncer.SyncerConfig{
			// LocalClusterID:   "tenant_" + namespace,
			LocalRestConfig:  cfg,
			LocalNamespace:   tenantNamespace,
			TenantRestConfig: config,
			TenantNamespace:  namespace,
		}, nil, nil)

		if err != nil {
			klog.Fatal(err)
		}

		go func() {

			if err = ovnSyncer.Start(stopCh); err != nil {
				klog.Fatalf("Error running the ovnkube syncer: %v", err)
			}
		}()

		<-stopCh
	}
	// start the leader election
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   defaultLeaseDuration,
		RenewDeadline:   defaultRenewDeadline,
		RetryPeriod:     defaultRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				becameLeader(ctx)
			},
			OnStoppedLeading: func() {
				klog.Infof("leader lost: %s", name)
				os.Exit(0)
			},
		},
	})
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
