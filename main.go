// Command u-l10n is the in-house localization service: the source of truth for
// mobile translation keys, the byte-exact export engine, and the over-the-air
// string delivery endpoint.
//
// See docs/ and the Confluence guide "u-l10n Backend Guide" for design detail.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli"

	ctxutil "github.com/yougroupteam/u-common-util/context"
	ulog "github.com/yougroupteam/u-common-util/log"

	"github.com/yougroupteam/u-l10n/pkg/config"
	"github.com/yougroupteam/u-l10n/pkg/lokalise"
	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/importsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/mergesvc"
	"github.com/yougroupteam/u-l10n/pkg/service/projectsvc"
	"github.com/yougroupteam/u-l10n/pkg/service/seed"
	"github.com/yougroupteam/u-l10n/pkg/service/usersvc"
)

const serviceName = "u-l10n"

var log = ulog.GetLogger(serviceName)

// Service is the fully-wired application graph, built by Wire.
type Service struct {
	Config   *config.Config
	Handler  http.Handler
	Seed     *seed.Service
	Tokens   repository.APITokenRepository
	Merge    *mergesvc.Service
	Import   *importsvc.Service
	Users    *usersvc.Service
	Projects *projectsvc.Service
}

func main() {
	ctx := ctxutil.NewContext(ctxutil.WithStan("main-process"))

	service, err := injectService(ctx)
	if err != nil {
		log.Fatale(ctx, "failed to initialise service", err)
	}

	app := cli.NewApp()
	app.Name = serviceName
	app.Usage = "In-house localization service by YouTech"
	app.Action = func(*cli.Context) error {
		return serve(ctx, service)
	}
	app.Commands = []cli.Command{
		seedFromFilesCommand(ctx, service),
		importCommand(ctx, service),
		tokenCommand(ctx, service),
		userCommand(ctx, service),
		projectCommand(ctx, service),
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatale(ctx, "service exited with error", err)
	}
}

// serve runs the HTTP server until SIGINT/SIGTERM, then drains in-flight
// requests within the configured shutdown timeout.
//
// The drain matters in Kubernetes: on rollout the pod receives SIGTERM at the
// same moment it is removed from the Service endpoints, and requests already in
// flight would otherwise be severed mid-response.
func serve(ctx context.Context, service *Service) error {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", service.Config.HTTPPort),
		Handler: service.Handler,

		// The chi Timeout middleware only bounds handler execution — it cannot
		// see a client that connects and then trickles its request headers in
		// (slowloris), because the handler has not started yet. These two are
		// the server-level backstop. Constants rather than config: nothing
		// deployment-specific hangs on them, and a knob nobody turns is only a
		// way to misconfigure the protection away.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Buffered so a signal arriving before we select is not dropped.
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		log.Infow(ctx, "http server listening",
			"port", service.Config.HTTPPort, "env", service.Config.Env)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server failed: %w", err)

	case sig := <-shutdown:
		log.Infow(ctx, "shutdown signal received, draining", "signal", sig.String())

		drainCtx, cancel := context.WithTimeout(ctx, service.Config.ShutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(drainCtx); err != nil {
			// Drain deadline exceeded: report it rather than pretending the
			// shutdown was clean, then let the process exit.
			return fmt.Errorf("graceful shutdown incomplete after %s: %w",
				service.Config.ShutdownTimeout, err)
		}

		log.Infow(ctx, "shutdown complete")
		return nil
	}
}

// seedFromFilesCommand loads the committed u-mobile tree into the database.
//
// This is the disaster-recovery path, the drift-reconciliation tool at cutover,
// and the reason the store and export engine can be proven before a Lokalise
// API token exists.
func seedFromFilesCommand(ctx context.Context, service *Service) cli.Command {
	var root, actor string
	var dryRun bool

	return cli.Command{
		Name:  "seed-from-files",
		Usage: "Load translations from a committed u-mobile tree",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:        "root",
				Usage:       "path to the u-mobile repository root",
				Destination: &root,
			},
			cli.StringFlag{
				Name:        "actor",
				Usage:       "email recorded as updated_by; writes to customer copy must be attributable",
				Destination: &actor,
			},
			cli.BoolFlag{
				// Runs the real pipeline in a transaction and rolls it back,
				// exercising every constraint for real while leaving nothing
				// behind.
				Name:        "dry-run",
				Usage:       "execute the full pipeline then roll back, reporting what would change",
				Destination: &dryRun,
			},
		},
		Action: func(*cli.Context) error {
			result, err := service.Seed.Run(ctx, seed.Options{
				Root:   root,
				Actor:  actor,
				DryRun: dryRun,
			})
			if err != nil {
				return err
			}

			for _, w := range result.Warnings {
				log.Infow(ctx, "seed warning", "detail", w)
			}
			log.Infow(ctx, "seed result",
				"dry_run", result.DryRun,
				"locales", result.LocalesSeen,
				"keys_upserted", result.KeysUpserted,
				"translations_written", result.TranslationsWritten,
				"warnings", len(result.Warnings))
			return nil
		},
	}
}

// importCommand imports the Lokalise project: values from the API, key
// presence from a directory of Lokalise file exports.
//
// Both sources are required because the API alone cannot distinguish a
// deliberately-blank translation from an untranslated one — see pkg/service/importsvc.
func importCommand(ctx context.Context, service *Service) cli.Command {
	var token, project, exportRoot, actor string
	var dryRun bool

	return cli.Command{
		Name:  "import",
		Usage: "Import the Lokalise project (values from the API, presence from a file export)",
		Flags: []cli.Flag{
			cli.StringFlag{
				// Prefer the environment variable: a token typed as a flag
				// lands in shell history and `ps` output, and a token is a
				// credential.
				Name:        "token",
				Usage:       "Lokalise API token (read scope); prefer $LOKALISE_API_TOKEN",
				EnvVar:      "LOKALISE_API_TOKEN",
				Destination: &token,
			},
			cli.StringFlag{
				Name:        "project",
				Usage:       "Lokalise project id",
				EnvVar:      "LOKALISE_PROJECT_ID",
				Destination: &project,
			},
			cli.StringFlag{
				Name:        "export-root",
				Usage:       "directory of Lokalise file exports; the presence oracle that keeps blank and untranslated apart",
				Destination: &exportRoot,
			},
			cli.StringFlag{
				Name:        "actor",
				Usage:       "email recorded as updated_by; writes to customer copy must be attributable",
				Destination: &actor,
			},
			cli.BoolFlag{
				Name:        "dry-run",
				Usage:       "execute the full pipeline then roll back, reporting what would change",
				Destination: &dryRun,
			},
		},
		Action: func(*cli.Context) error {
			if token == "" || project == "" {
				return errors.New("import: --token (or $LOKALISE_API_TOKEN) and --project are required")
			}

			client := lokalise.New(token, project)
			defer client.Close()

			result, err := service.Import.Run(ctx, client, importsvc.Options{
				ExportRoot: exportRoot,
				Actor:      actor,
				DryRun:     dryRun,
			})
			if err != nil {
				return err
			}

			for _, w := range result.Warnings {
				log.Infow(ctx, "import warning", "detail", w)
			}
			log.Infow(ctx, "import result",
				"dry_run", result.DryRun,
				"keys_upserted", result.KeysUpserted,
				"translations_written", result.TranslationsWritten,
				"translations_skipped", result.TranslationsSkipped,
				"warnings", len(result.Warnings))
			return nil
		},
	}
}

// tokenCommand issues and revokes API tokens for scripts and CI.
func tokenCommand(ctx context.Context, service *Service) cli.Command {
	var name, scope, actor string

	return cli.Command{
		Name:  "token",
		Usage: "Manage API tokens for scripts and CI",
		Subcommands: []cli.Command{
			{
				Name:  "create",
				Usage: "Issue a token; the plaintext is shown ONCE and is never recoverable",
				Flags: []cli.Flag{
					cli.StringFlag{Name: "name", Usage: "identifier, e.g. u-mobile-ci", Destination: &name},
					cli.StringFlag{Name: "scope", Value: "read_export",
						Usage: "read_export or read_write", Destination: &scope},
					cli.StringFlag{Name: "actor", Usage: "email of the issuer", Destination: &actor},
				},
				Action: func(*cli.Context) error {
					if name == "" || actor == "" {
						return errors.New("token create: --name and --actor are required")
					}
					plaintext, token, err := service.Tokens.Create(ctx, nil, name, scope, actor, nil)
					if err != nil {
						return err
					}
					// Printed to stdout, deliberately NOT logged: the log ships
					// to a central store where a live credential must never
					// land. This is the only moment the plaintext exists
					// outside the caller's terminal.
					fmt.Printf("token %q created with scope %s\n", token.Name, token.Scope)
					fmt.Printf("\n  %s\n\n", plaintext)
					fmt.Println("Store it now. Only its SHA-256 is kept, so it cannot be shown again.")
					return nil
				},
			},
			{
				Name:  "revoke",
				Usage: "Revoke a token by name",
				Flags: []cli.Flag{
					cli.StringFlag{Name: "name", Destination: &name},
					cli.StringFlag{Name: "actor", Usage: "email of the revoker", Destination: &actor},
				},
				Action: func(*cli.Context) error {
					if name == "" || actor == "" {
						return errors.New("token revoke: --name and --actor are required")
					}
					if err := service.Tokens.Revoke(ctx, nil, name, actor); err != nil {
						return err
					}
					log.Infow(ctx, "token revoked", "name", name, "by", actor)
					return nil
				},
			},
		},
	}
}

// userCommand creates and promotes portal operators from the shell.
//
// This exists to solve a bootstrapping problem that has no in-band answer.
// PATCH /api/v1/admin/users/{email}/role requires the admin role, and roles
// live in the users table — so on a fresh database nobody is an admin, nobody
// can be made one through the API, and the portal is permanently locked out of
// itself. Something outside the authorization system has to start the chain.
//
// The shell is the right place for it. Running this command requires access to
// the deployed pod and its database credentials, which is strictly more
// privilege than any role in this table can express: anyone who can run it
// could already have written the row by hand with psql. This just makes the
// supported way to do it the easy way, and — unlike psql — it validates the
// role and writes an audit_events row, so the first grant is as traceable as
// every later one. The alternative designs are worse: a seeded admin address
// baked into a migration is a credential in git that nobody remembers to
// remove, and an env-var "superuser" is a permanent, invisible bypass of the
// entire role model.
//
//	u-l10n user grant --email ashik.saini@you.co --role admin --actor ashik.saini@you.co
func userCommand(ctx context.Context, service *Service) cli.Command {
	var email, role, status, actor string
	var platformAdmin bool

	return cli.Command{
		Name:  "user",
		Usage: "Manage portal operators; this is the only way to create the first admin",
		Subcommands: []cli.Command{
			{
				Name:  "grant",
				Usage: "Create an operator or change their role; idempotent",
				Flags: []cli.Flag{
					cli.StringFlag{Name: "email", Usage: "the operator's Google Workspace address", Destination: &email},
					cli.StringFlag{Name: "role", Value: "viewer",
						Usage: "viewer, editor, approver or admin", Destination: &role},
					cli.StringFlag{Name: "status", Value: "active",
						Usage: "active or disabled", Destination: &status},
					cli.BoolFlag{
						// The one privilege that is not scoped to a project: creating
						// a project and granting its first role. Without this flag
						// there is no way to mint the first platform admin, and the
						// API can never bootstrap itself — see requirePlatformAdmin
						// in route/identity.go.
						Name:        "platform-admin",
						Usage:       "also grant the platform-admin privilege (create projects, grant first roles)",
						Destination: &platformAdmin,
					},
					cli.StringFlag{Name: "actor",
						Usage:       "email of whoever is running this; recorded in the audit trail",
						Destination: &actor},
				},
				Action: func(*cli.Context) error {
					if email == "" || actor == "" {
						return errors.New("user grant: --email and --actor are required")
					}
					user, err := service.Users.Grant(ctx, email, role, status, platformAdmin, actor, "cli")
					if err != nil {
						return err
					}
					fmt.Printf("%s is now %s (%s, platform_admin=%t)\n",
						user.Email, user.Role, user.Status, user.IsPlatformAdmin)
					return nil
				},
			},
			{
				Name:  "list",
				Usage: "List operators and their roles",
				Action: func(*cli.Context) error {
					users, err := service.Users.List(ctx)
					if err != nil {
						return err
					}
					if len(users) == 0 {
						// The state this command exists to get you out of, said
						// plainly rather than as an empty table.
						fmt.Println("no users yet — nobody can sign in to the portal")
						fmt.Println("create the first admin with: u-l10n user grant --email <you> --role admin --actor <you>")
						return nil
					}
					for _, u := range users {
						fmt.Printf("%-40s %-9s %s\n", u.Email, u.Role, u.Status)
					}
					return nil
				},
			},
		},
	}
}

// projectCommand mints and lists projects from the shell.
//
// It exists for the same bootstrapping reason userCommand does: creating a
// project needs a platform admin, and on a fresh database — or one where
// nobody wants to expose project creation over the API yet — the shell is
// the way in. It shares the same escape-hatch reasoning: anyone who can run
// this command already has database access, which is strictly more privilege
// than any role this schema can express.
//
//	u-l10n project create --code youbiz --name YouBiz --actor ashik.saini@you.co
func projectCommand(ctx context.Context, service *Service) cli.Command {
	var code, name, lokaliseProjectID, actor string
	var includeArchived bool

	return cli.Command{
		Name:  "project",
		Usage: "Manage projects",
		Subcommands: []cli.Command{
			{
				Name:  "create",
				Usage: "Mint a project and grant its creator the admin role on it",
				Flags: []cli.Flag{
					cli.StringFlag{Name: "code", Usage: "URL slug, e.g. youbiz", Destination: &code},
					cli.StringFlag{Name: "name", Usage: "display name, e.g. YouBiz", Destination: &name},
					cli.StringFlag{Name: "lokalise-project-id",
						Usage: "Lokalise project id this project imports from, if any", Destination: &lokaliseProjectID},
					cli.StringFlag{Name: "actor",
						Usage:       "email of whoever is running this; becomes the project's first admin",
						Destination: &actor},
				},
				Action: func(*cli.Context) error {
					if actor == "" {
						return errors.New("project create: --actor is required")
					}
					p, err := service.Projects.Create(ctx, actor, projectsvc.NewProject{
						Code:              code,
						Name:              name,
						LokaliseProjectID: lokaliseProjectID,
					})
					if err != nil {
						return err
					}
					fmt.Printf("project %q (id %d) created; %s granted admin\n", p.Code, p.ID, actor)
					return nil
				},
			},
			{
				Name:  "list",
				Usage: "List projects",
				Flags: []cli.Flag{
					cli.BoolFlag{Name: "all", Usage: "include archived projects", Destination: &includeArchived},
				},
				Action: func(*cli.Context) error {
					projects, err := service.Projects.List(ctx, includeArchived)
					if err != nil {
						return err
					}
					if len(projects) == 0 {
						fmt.Println("no projects yet")
						return nil
					}
					for _, p := range projects {
						fmt.Printf("%-4d %-20s %-10s %s\n", p.ID, p.Code, p.Status, p.Name)
					}
					return nil
				},
			},
		},
	}
}
