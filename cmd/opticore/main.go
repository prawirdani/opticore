// Command opticore is the Opticore daemon and its operator CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prawirdani/opticore/internal/acs"
	"github.com/prawirdani/opticore/internal/api"
	"github.com/prawirdani/opticore/internal/config"
	"github.com/prawirdani/opticore/internal/cwmp"
	"github.com/prawirdani/opticore/internal/hook"
	"github.com/prawirdani/opticore/internal/store"
	"github.com/prawirdani/opticore/internal/wake"
)

const usage = `usage: opticore <command> [args]

  serve                                 run the daemon
  migrate                               apply database migrations
  devices [--json] [--serial P] [--class C]
  device <serial|id> [--json] [--prefix P]
  tasks <serial|id> [--json]
  task <serial|id> get <path>...
  task <serial|id> set <path> <value> [type]
  task <serial|id> reboot
  wake <serial|id>
  token create <name> --scope read,write[,reveal]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = withStore(ctx, func(s *store.Store, _ config.Config) error { return s.Migrate(ctx) })
	case "devices":
		err = cmdDevices(ctx, args)
	case "device":
		err = cmdDevice(ctx, args)
	case "tasks":
		err = cmdTasks(ctx, args)
	case "task":
		err = cmdTask(ctx, args)
	case "wake":
		err = cmdWake(ctx, args)
	case "token":
		err = cmdToken(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		if store.IsMissingTable(err) {
			err = fmt.Errorf("%w (database schema missing: run `opticore migrate`)", err)
		}
		fmt.Fprintln(os.Stderr, "opticore:", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func serve(ctx context.Context) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	log := newLogger(cfg.LogLevel)

	st, err := store.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	st.TaskExpiry = cfg.TaskExpiry
	if _, err := st.AcquireLeader(ctx); err != nil {
		return err
	}
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if v, err := st.SchemaVersion(ctx); err == nil {
		log.Info("database schema", "version", v)
	}
	log.Info("cwmp device security", "allow_cidrs", len(cfg.CWMPAllowCIDRs),
		"cr_url_match_source", cfg.CWMPCRMatchSource, "device_auth", cfg.CWMPAuthUsername != "")

	svc := &acs.Service{Store: st, Key: cfg.SecretKey, Waker: wake.New(cfg.CWMPCRTimeout, log), Logger: log}
	sessions := &acs.Sessions{
		Store: st, Svc: svc, Key: cfg.SecretKey, Logger: log,
		GPVBatch: cfg.CWMPGPVBatch, MaxAttempts: cfg.TaskMaxAttempts,
		CRMatchSource: cfg.CWMPCRMatchSource,
		HookEvents:    cfg.HookEvents, HookPrefetch: cfg.HookPrefetch, HookTimeout: cfg.HookTimeout,
	}
	if cfg.HookURL != "" {
		hc, err := hook.New(cfg.HookURL, cfg.HookSecret, cfg.HookStepTimeout)
		if err != nil {
			return err
		}
		sessions.Hook = hc
		log.Info("inform hook configured", "url", cfg.HookURL, "events", cfg.HookEvents,
			"step_timeout", cfg.HookStepTimeout, "timeout", cfg.HookTimeout)
	}
	cs := &cwmp.Server{
		Handler:        sessions,
		Logger:         log,
		MaxBody:        cfg.CWMPMaxBody,
		SessionTimeout: cfg.CWMPSessionTimeout,
		MaxSessions:    cfg.CWMPMaxSessions,
		Allow:          cfg.CWMPAllowCIDRs,
	}
	if cfg.CWMPAuthUsername != "" {
		cs.Auth = cwmp.NewAuth(cfg.CWMPAuthUsername, cfg.CWMPAuthPassword)
	}
	cs.Start()
	defer cs.Stop()

	cwmpMux := http.NewServeMux()
	cwmpMux.Handle("/", cs)
	cwmpMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		health(w, r, st)
	})
	srv := &http.Server{
		Addr:              cfg.CWMPAddr,
		Handler:           cwmpMux,
		ConnContext:       cs.ConnContext,
		ReadHeaderTimeout: cfg.CWMPRequestTimeout,
		ReadTimeout:       cfg.CWMPRequestTimeout,
		WriteTimeout:      cfg.CWMPRequestTimeout,
		IdleTimeout:       cfg.CWMPSessionTimeout,
	}

	apiSrv := api.New(st, svc, cfg.SecretKey, log, api.Options{
		AllowInsecureLocal: cfg.APIInsecureLocal,
		RequireRevealScope: cfg.APIRequireReveal,
		ListenAddr:         cfg.APIAddr,
	})
	if err := apiSrv.Err(); err != nil {
		return err
	}
	apiMux := http.NewServeMux()
	apiMux.Handle("/", apiSrv.Handler())
	apiMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		health(w, r, st)
	})
	apiHTTP := &http.Server{
		Addr:              cfg.APIAddr,
		Handler:           apiMux,
		ReadHeaderTimeout: cfg.CWMPRequestTimeout,
		IdleTimeout:       cfg.CWMPSessionTimeout,
	}

	janitorCtx, cancelJanitor := context.WithCancel(ctx)
	defer cancelJanitor()
	go janitor(janitorCtx, st, log, cfg.TaskRetention, cfg.EventRetention)

	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- apiHTTP.ListenAndServe() }()
	log.Info("opticore listening", "cwmp", cfg.CWMPAddr, "api", cfg.APIAddr,
		"api_auth", !cfg.APIInsecureLocal)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.CWMPSessionTimeout)
	defer cancel()
	log.Info("shutting down")
	apiSrv.Close()
	apiErr := apiHTTP.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return apiErr
}

func health(w http.ResponseWriter, r *http.Request, st *store.Store) {
	out := map[string]any{"db": "ok"}
	code := http.StatusOK
	if err := st.Ping(r.Context()); err != nil {
		out["db"], code = err.Error(), http.StatusServiceUnavailable
	} else if t, err := st.NewestInform(r.Context()); err == nil && !t.IsZero() {
		out["newest_inform_age_s"] = int(time.Since(t).Seconds())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(out)
}

func janitor(ctx context.Context, st *store.Store, log *slog.Logger, taskRetention, eventRetention time.Duration) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if n, err := st.ExpireTasks(ctx); err != nil {
				log.Error("janitor: expire tasks", "err", err)
			} else if n > 0 {
				log.Info("janitor: expired tasks", "count", n)
			}
			if now.Sub(lastPurge) < time.Hour {
				continue
			}
			lastPurge = now
			if nt, ne, err := st.PurgeOld(ctx, taskRetention, eventRetention); err != nil {
				log.Error("janitor: purge", "err", err)
			} else if nt+ne > 0 {
				log.Info("janitor: purged old rows", "tasks", nt, "events", ne)
			}
		}
	}
}

func withStore(ctx context.Context, fn func(*store.Store, config.Config) error) error {
	cfg, err := config.LoadClient(os.Getenv)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBDSN)
	if err != nil {
		return err
	}
	defer st.Close()
	return fn(st, cfg)
}

func service(st *store.Store, cfg config.Config) *acs.Service {
	log := newLogger(cfg.LogLevel)
	return &acs.Service{Store: st, Key: cfg.SecretKey, Waker: wake.New(2*time.Second, log), Logger: log}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// split separates flags from positional args, so flags may follow them.
func split(args []string) (flags, pos []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && a != "--json" && a != "-json" && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return flags, pos
}

func cmdDevices(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	serial := fs.String("serial", "", "serial prefix")
	class := fs.String("class", "", "product class")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		ds, err := st.ListDevices(ctx, store.ListFilter{SerialPrefix: *serial, ProductClass: *class, Limit: 1000})
		if err != nil {
			return err
		}
		if *asJSON {
			for i := range ds {
				redact(&ds[i])
			}
			return printJSON(ds)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSERIAL\tCLASS\tVERSION\tLAST INFORM\tADDR\tCR")
		for _, d := range ds {
			cr := "no"
			if d.CRSetAt != nil {
				cr = "yes"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s ago\t%s\t%s\n", d.ID, d.Serial, d.ProductClass,
				d.SoftwareVersion, time.Since(d.LastInform).Round(time.Second), d.LastAddr, cr)
		}
		return w.Flush()
	})
}

func redact(d *store.Device) {
	if d.CRPassword != "" {
		d.CRPassword = "••••"
	}
}

func cmdDevice(ctx context.Context, args []string) error {
	fl, pos := split(args)
	fs := flag.NewFlagSet("device", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	prefix := fs.String("prefix", "", "only parameters under this path")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: device <serial|id> [--json] [--prefix P]")
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		d, err := st.GetDevice(ctx, pos[0])
		if err != nil {
			return err
		}
		redact(&d)
		for k := range d.Params {
			if !strings.HasPrefix(k, *prefix) {
				delete(d.Params, k)
			}
		}
		if *asJSON {
			return printJSON(d)
		}
		fmt.Printf("id: %s\nserial: %s\noui: %s\nclass: %s\nversion: %s\nlast inform: %s (%s ago)\naddr: %s\ncr url: %s\ncr set: %v\npending reboot: %v\n",
			d.ID, d.Serial, d.OUI, d.ProductClass, d.SoftwareVersion, d.LastInform.Format(time.RFC3339),
			time.Since(d.LastInform).Round(time.Second), d.LastAddr, d.CRURL, d.CRSetAt != nil, d.PendingReboot)
		if len(d.LastFault) > 0 {
			fmt.Printf("last fault: %s\n", d.LastFault)
		}
		keys := make([]string, 0, len(d.Params))
		for k := range d.Params {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		fmt.Println("params:")
		for _, k := range keys {
			p := d.Params[k]
			v := p.Value
			if p.Secret {
				v = "••••"
			}
			fmt.Printf("  %s = %s (%s)\n", k, v, p.Type)
		}
		return nil
	})
}

func cmdTasks(ctx context.Context, args []string) error {
	fl, pos := split(args)
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: tasks <serial|id> [--json]")
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		ts, err := st.ListTasks(ctx, pos[0], 100)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(ts)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tSTATUS\tATTEMPTS\tCREATED\tFAULT\tRESULT")
		for _, t := range ts {
			fault := t.Fault
			if t.FaultCode != nil {
				fault = fmt.Sprintf("%d %s", *t.FaultCode, t.Fault)
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%s\t%s\t%s\n", t.ID, t.Name, t.Status, t.Attempts,
				t.CreatedAt.Local().Format("01-02 15:04:05"), fault, t.Result)
		}
		return w.Flush()
	})
}

func cmdTask(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: task <serial|id> get <path>... | set <path> <value> [type] | reboot")
	}
	dev, name, rest := args[0], args[1], args[2:]
	var t store.NewTask
	switch name {
	case "get":
		if len(rest) == 0 {
			return errors.New("usage: task <serial|id> get <path>...")
		}
		t = store.NewTask{Name: "get", Args: mustJSON(acs.GetArgs{Paths: rest})}
	case "set":
		if len(rest) < 2 || len(rest) > 3 {
			return errors.New("usage: task <serial|id> set <path> <value> [type]")
		}
		t = store.NewTask{Name: "set", Args: mustJSON(acs.SetArgs{Values: [][]string{rest}})}
	case "reboot":
		t = store.NewTask{Name: "reboot"}
	default:
		return fmt.Errorf("unknown task %q", name)
	}
	return withStore(ctx, func(st *store.Store, cfg config.Config) error {
		out, err := service(st, cfg).Enqueue(ctx, dev, []store.NewTask{t})
		if err != nil {
			return err
		}
		for _, t := range out {
			fmt.Printf("queued task %d (%s) for %s\n", t.ID, t.Name, t.DeviceID)
		}
		return nil
	})
}

func cmdWake(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: wake <serial|id>")
	}
	return withStore(ctx, func(st *store.Store, cfg config.Config) error {
		if err := service(st, cfg).Wake(ctx, args[0]); err != nil {
			return err
		}
		fmt.Println("connection request accepted")
		return nil
	})
}

func cmdToken(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "create" {
		return errors.New("usage: token create <name> --scope read,write[,reveal]")
	}
	name := args[1]
	fl, _ := split(args[2:])
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	scopes := fs.String("scope", "read", "comma-separated scopes: read, write, reveal")
	if err := fs.Parse(fl); err != nil {
		return err
	}
	sc := strings.Split(*scopes, ",")
	for i := range sc {
		sc[i] = strings.TrimSpace(sc[i])
		switch sc[i] {
		case "read", "write", "reveal":
		default:
			return fmt.Errorf("unknown scope %q", sc[i])
		}
	}
	return withStore(ctx, func(st *store.Store, _ config.Config) error {
		tok, err := st.CreateToken(ctx, name, sc)
		if err != nil {
			return err
		}
		fmt.Printf("token %q created with scopes %s\n%s\n(shown once)\n", name, strings.Join(sc, ","), tok)
		return nil
	})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
