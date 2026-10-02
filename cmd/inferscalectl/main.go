package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/admin"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/config"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/inferscale/inferscale/internal/storage/postgres"
	"github.com/inferscale/inferscale/internal/tenant"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var version = "dev"

type operatorRuntime struct {
	config config.Config
	store  *postgres.Store
	kube   client.Client
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	runtime := &operatorRuntime{config: cfg}
	defer runtime.close()
	return newRootCommand(runtime).ExecuteContext(ctx)
}

func newRootCommand(runtime *operatorRuntime) *cobra.Command {
	root := &cobra.Command{
		Use:           "inferscalectl",
		Short:         "Operator administration for InferScale",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.AddCommand(
		newTenantCommand(runtime),
		newAPIKeyCommand(runtime),
		newRolloutCommand(runtime),
		newProfileCommand(runtime),
	)
	return root
}

func newTenantCommand(runtime *operatorRuntime) *cobra.Command {
	command := &cobra.Command{Use: "tenant", Short: "Manage tenants and their isolated namespaces"}

	var create struct {
		slug, name, priority                              string
		maxDeployments, maxGPUs, rate, concurrent, queued int32
	}
	createCommand := &cobra.Command{
		Use:   "create",
		Short: "Create a tenant and provision its namespace baseline",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := runtime.openStore(cmd.Context())
			if err != nil {
				return err
			}
			kube, err := runtime.openKube()
			if err != nil {
				return err
			}
			repository := postgres.NewTenantRepository(store)
			service := tenant.NewService(repository, tenant.WithNamespaceProvisioner(tenant.NamespaceProvisioner{
				Client: kube, FieldOwner: "inferscalectl",
				KubernetesAPIServerCIDRs: runtime.config.KubernetesAPICIDRs,
				KubernetesAPIServerPort:  int32(runtime.config.KubernetesAPIPort),
			}))
			value, err := (admin.TenantAdmin{Service: service, Repository: repository}).Create(cmd.Context(), create.slug, create.name, tenant.Quota{
				MaxDeployments: create.maxDeployments, MaxGPUs: create.maxGPUs,
				RequestsPerMinute: create.rate, MaxConcurrentRequests: create.concurrent,
				MaxQueuedRequests: create.queued, DefaultPriorityClass: create.priority,
			})
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), value)
		},
	}
	createCommand.Flags().StringVar(&create.slug, "slug", "", "unique DNS-safe tenant slug")
	createCommand.Flags().StringVar(&create.name, "name", "", "tenant display name")
	createCommand.Flags().Int32Var(&create.maxDeployments, "max-deployments", 0, "maximum deployment count")
	createCommand.Flags().Int32Var(&create.maxGPUs, "max-gpus", 0, "maximum total exclusive GPUs")
	createCommand.Flags().Int32Var(&create.rate, "requests-per-minute", 0, "tenant-wide inference request rate")
	createCommand.Flags().Int32Var(&create.concurrent, "max-concurrent-requests", 0, "tenant-wide concurrent request budget")
	createCommand.Flags().Int32Var(&create.queued, "max-queued-requests", 0, "tenant-wide queued request budget")
	createCommand.Flags().StringVar(&create.priority, "default-priority", "standard", "interactive, standard, or batch")
	for _, flag := range []string{"slug", "name", "max-deployments", "max-gpus", "requests-per-minute", "max-concurrent-requests", "max-queued-requests"} {
		_ = createCommand.MarkFlagRequired(flag)
	}

	var listLimit, listOffset int
	var listOutput string
	listCommand := &cobra.Command{
		Use:   "list",
		Short: "List tenants",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := runtime.openStore(cmd.Context())
			if err != nil {
				return err
			}
			values, err := (admin.TenantAdmin{Repository: postgres.NewTenantRepository(store)}).List(cmd.Context(), listLimit, listOffset)
			if err != nil {
				return err
			}
			if listOutput == "json" {
				return writeJSON(cmd.OutOrStdout(), values)
			}
			if listOutput != "table" {
				return fmt.Errorf("output must be table or json")
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tSLUG\tNAMESPACE\tMAX GPUS\tSUSPENDED")
			for _, value := range values {
				_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%t\n", value.ID, value.Slug, value.Namespace, value.Quota.MaxGPUs, value.SuspendedAt != nil)
			}
			return writer.Flush()
		},
	}
	listCommand.Flags().IntVar(&listLimit, "limit", 50, "page size (maximum 200)")
	listCommand.Flags().IntVar(&listOffset, "offset", 0, "operator list offset")
	listCommand.Flags().StringVarP(&listOutput, "output", "o", "table", "table or json")

	command.AddCommand(createCommand, listCommand)
	command.AddCommand(tenantStateCommand(runtime, "suspend"), tenantStateCommand(runtime, "resume"), tenantStateCommand(runtime, "provision"))
	return command
}

func tenantStateCommand(runtime *operatorRuntime, action string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " TENANT_ID",
		Short: action + " a tenant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := runtime.openStore(cmd.Context())
			if err != nil {
				return err
			}
			repository := postgres.NewTenantRepository(store)
			var options []tenant.ServiceOption
			if action == "provision" {
				kube, kubeErr := runtime.openKube()
				if kubeErr != nil {
					return kubeErr
				}
				options = append(options, tenant.WithNamespaceProvisioner(tenant.NamespaceProvisioner{
					Client: kube, FieldOwner: "inferscalectl",
					KubernetesAPIServerCIDRs: runtime.config.KubernetesAPICIDRs,
					KubernetesAPIServerPort:  int32(runtime.config.KubernetesAPIPort),
				}))
			}
			operator := admin.TenantAdmin{Service: tenant.NewService(repository, options...), Repository: repository}
			switch action {
			case "suspend":
				err = operator.Suspend(cmd.Context(), args[0])
			case "resume":
				err = operator.Resume(cmd.Context(), args[0])
			case "provision":
				err = operator.Provision(cmd.Context(), args[0])
			}
			if err == nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", action+"d")
			}
			return err
		},
	}
}

func newAPIKeyCommand(runtime *operatorRuntime) *cobra.Command {
	command := &cobra.Command{Use: "apikey", Short: "Issue, rotate, and revoke tenant API keys"}
	type keyFlags struct {
		tenantID, name, expiry string
		scopes                 []string
	}
	addFlags := func(command *cobra.Command, flags *keyFlags) {
		command.Flags().StringVar(&flags.tenantID, "tenant", "", "tenant UUID")
		command.Flags().StringVar(&flags.name, "name", "", "operator-visible key name")
		command.Flags().StringSliceVar(&flags.scopes, "scope", nil, "allowed scope (repeatable or comma-separated)")
		command.Flags().StringVar(&flags.expiry, "expires-at", "", "optional RFC3339 expiration")
		_ = command.MarkFlagRequired("tenant")
		_ = command.MarkFlagRequired("name")
	}
	issue := keyFlags{}
	issueCommand := &cobra.Command{
		Use:   "issue",
		Short: "Issue a key; its secret is displayed exactly once",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key, raw, err := issueKey(cmd.Context(), runtime, issue)
			if err != nil {
				return err
			}
			return writeJSON(cmd.OutOrStdout(), map[string]any{"id": key.ID, "apiKey": raw, "scopes": key.Scopes, "expiresAt": key.ExpiresAt})
		},
	}
	addFlags(issueCommand, &issue)

	var revokeTenant string
	revokeCommand := &cobra.Command{
		Use:   "revoke KEY_ID",
		Short: "Revoke a tenant API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := runtime.openStore(cmd.Context())
			if err != nil {
				return err
			}
			return (admin.APIKeyAdmin{Service: auth.NewService(postgres.NewAuthRepository(store))}).Revoke(cmd.Context(), revokeTenant, args[0])
		},
	}
	revokeCommand.Flags().StringVar(&revokeTenant, "tenant", "", "tenant UUID")
	_ = revokeCommand.MarkFlagRequired("tenant")

	rotate := keyFlags{}
	rotateCommand := &cobra.Command{
		Use:   "rotate OLD_KEY_ID",
		Short: "Issue a replacement key, display it, then revoke the old key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, raw, err := issueKey(cmd.Context(), runtime, rotate)
			if err != nil {
				return err
			}
			// Print the only copy before revocation. If revocation fails the new
			// credential remains usable and the operator can safely retry it.
			if err := writeJSON(cmd.OutOrStdout(), map[string]any{"id": key.ID, "apiKey": raw, "scopes": key.Scopes, "expiresAt": key.ExpiresAt}); err != nil {
				return err
			}
			store, err := runtime.openStore(cmd.Context())
			if err != nil {
				return err
			}
			if err := (admin.APIKeyAdmin{Service: auth.NewService(postgres.NewAuthRepository(store))}).Revoke(cmd.Context(), rotate.tenantID, args[0]); err != nil {
				return fmt.Errorf("replacement key was issued but old key revocation failed: %w", err)
			}
			return nil
		},
	}
	addFlags(rotateCommand, &rotate)
	command.AddCommand(issueCommand, rotateCommand, revokeCommand)
	return command
}

func issueKey(ctx context.Context, runtime *operatorRuntime, flags struct {
	tenantID, name, expiry string
	scopes                 []string
}) (*auth.APIKey, string, error) {
	store, err := runtime.openStore(ctx)
	if err != nil {
		return nil, "", err
	}
	var expiresAt *time.Time
	if flags.expiry != "" {
		parsed, parseErr := time.Parse(time.RFC3339, flags.expiry)
		if parseErr != nil {
			return nil, "", fmt.Errorf("parse --expires-at: %w", parseErr)
		}
		expiresAt = &parsed
	}
	return (admin.APIKeyAdmin{Service: auth.NewService(postgres.NewAuthRepository(store))}).Issue(ctx, flags.tenantID, flags.name, flags.scopes, expiresAt)
}

func newRolloutCommand(runtime *operatorRuntime) *cobra.Command {
	command := &cobra.Command{Use: "rollout", Short: "Apply operator rollout overrides"}
	for _, action := range []admin.RolloutAction{admin.RolloutPause, admin.RolloutResume, admin.RolloutAbort} {
		action := action
		var tenantID string
		subcommand := &cobra.Command{
			Use:   string(action) + " DEPLOYMENT_ID",
			Short: string(action) + " a progressive rollout",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				store, err := runtime.openStore(cmd.Context())
				if err != nil {
					return err
				}
				kube, err := runtime.openKube()
				if err != nil {
					return err
				}
				return (admin.RolloutAdmin{Deployments: postgres.NewDeploymentRepository(store), Client: kube}).Set(cmd.Context(), tenantID, args[0], action)
			},
		}
		subcommand.Flags().StringVar(&tenantID, "tenant", "", "tenant UUID")
		_ = subcommand.MarkFlagRequired("tenant")
		command.AddCommand(subcommand)
	}
	return command
}

func newProfileCommand(runtime *operatorRuntime) *cobra.Command {
	command := &cobra.Command{Use: "profile", Short: "Approve or revoke measured backend profiles"}
	for _, action := range []string{"approve", "revoke"} {
		action := action
		var operator string
		subcommand := &cobra.Command{
			Use:   action + " PROFILE_ID",
			Short: action + " a runtime profile for backend:auto",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				store, err := runtime.openStore(cmd.Context())
				if err != nil {
					return err
				}
				return (admin.ProfileAdmin{Repository: postgres.NewRuntimeProfileRepository(store)}).SetEligibility(cmd.Context(), args[0], operator, action == "approve")
			},
		}
		subcommand.Flags().StringVar(&operator, "operator", "", "approving/revoking operator identity")
		_ = subcommand.MarkFlagRequired("operator")
		command.AddCommand(subcommand)
	}
	return command
}

func (r *operatorRuntime) openStore(ctx context.Context) (*postgres.Store, error) {
	if r.store != nil {
		return r.store, nil
	}
	if strings.TrimSpace(r.config.DatabaseURL) == "" {
		return nil, errors.New("INFERSCALE_DATABASE_URL is required")
	}
	store, err := postgres.Open(ctx, r.config.DatabaseURL)
	if err != nil {
		return nil, err
	}
	r.store = store
	return store, nil
}

func (r *operatorRuntime) openKube() (client.Client, error) {
	if r.kube != nil {
		return r.kube, nil
	}
	restConfig, err := kubeutil.RESTConfig(r.config.Kubeconfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, networkingv1.AddToScheme, platformv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	value, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	r.kube = value
	return value, nil
}

func (r *operatorRuntime) close() {
	if r.store != nil {
		r.store.Close()
	}
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
