// Command celld-operator provisions experimental celld fleet infrastructure.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/go-logr/logr"

	fleet "github.com/ewhauser/celld-operator/api/v1alpha1"
	"github.com/ewhauser/celld-operator/internal/controller"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "celld-operator:", err)
		os.Exit(1)
	}
}
func run() error {
	fs := flag.NewFlagSet("celld-operator", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "Print version and exit")
	namespace := fs.String("operator-namespace", "celld-system", "Namespace of trusted operator pods and leader election")
	enforced := fs.Bool("network-policy-enforced", false, "Administrator attests NetworkPolicy enforcement has been verified on this cluster")
	localTest := fs.Bool("local-test", false, "Use disposable local MinIO test configuration; never enable on EKS")
	metrics := fs.String("metrics-bind-address", "0", "Optional metrics listener (0 disables)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil)))
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := fleet.AddToScheme(scheme); err != nil {
		return err
	}
	config, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, LeaderElection: true, LeaderElectionID: "celld-operator.celld.example.com", LeaderElectionNamespace: *namespace, Metrics: metricsserver.Options{BindAddress: *metrics}, HealthProbeBindAddress: ":8082"})
	if err != nil {
		return err
	}
	direct, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	collector, err := controller.NewCollector(direct, config)
	if err != nil {
		return err
	}
	reconciler := &controller.Reconciler{Collector: collector, Client: direct, Options: controller.Options{OperatorNamespace: *namespace, LocalTest: *localTest}, NetworkPolicyEnforced: *enforced}
	// Local disposable mode never falls through to AWS credentials or endpoints.
	if !*localTest {
		reconciler.Evidence = controller.NewProductionEvidence(direct)
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctrl.SetupSignalHandler())
}
