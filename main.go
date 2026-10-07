// AryxD Agent (binary and service still named EMLyUpdater, see AGENTS.md §
// "Product name vs. technical identifiers") is a standalone Windows service
// (LocalSystem, auto-start): the distribution, monitoring and management agent
// for 3gIT's supported products, today EMLy. It keeps EMLy up to date on
// domain-joined machines: it polls an update
// manifest over HTTP, downloads and SHA256-verifies the InnoSetup installer,
// and applies it silently - immediately when EMLy is closed, on exit when it
// is open, or force-killing it for critical updates.
//
// Subcommands:
//
//	install     register the auto-start service + Event Log source (admin)
//	uninstall   stop and remove the service (ProgramData state is kept)
//	start       start the service
//	stop        stop the service
//	run         run the update loop in the foreground (debug)
//	show-toast  display the update-complete notification (internal use: the
//	            SYSTEM service re-launches itself with this subcommand inside
//	            the console user's session, see internal/notify.LaunchToast)
//	show-progress  display the download/install progress window (internal
//	            use: started by the service in the console user's session,
//	            driven through its stdin, see internal/notify.OpenProgressWindow)
//	restart-service  stop then start the service (internal use: launched
//	            detached by the service itself for the client channel's
//	            service.restart command)
//	simulate-crash [panic|goroutine|error]  fail on purpose, to check the
//	            fatal-error box (internal/crash); panic is the default
//
// Without arguments the binary expects to be launched by the SCM.
//
//go:generate goversioninfo -64
//go:generate go run ./tools/genversion
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"emlyupdater/internal/cert"
	"emlyupdater/internal/config"
	"emlyupdater/internal/crash"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/progresswin"
	"emlyupdater/internal/service"
	"emlyupdater/internal/toast"
	"emlyupdater/internal/tray"
	"emlyupdater/internal/version"
)

const (
	// productName is the display name only. Every technical identifier -
	// service name, pipe, mutex, ProgramData directory, Event Log source,
	// exe and installer file names, User-Agent - keeps "EMLyUpdater": the
	// emly and emly-go-api repos and already-installed machines depend on
	// them (AGENTS.md § "Product name vs. technical identifiers").
	productName = version.ProductName
	displayName = productName + " Service"
	description = "AryxD Agent: agente di distribuzione, monitoraggio e gestione per i prodotti supportati da 3gIT. "

	// supportedProducts are the products this agent distributes, logged at
	// startup so a log read in isolation says what the agent is for.
	supportedProducts = "EMLy"
	agentRoles        = "distribution, monitoring, management"
)

// logIdentity writes the agent's identity as the first lines of a run.
func logIdentity(log *logging.Logger, mode string) {
	log.Info(productName+" "+mode+" starting",
		"version", version.Version,
		"roles", agentRoles,
		"supportedProducts", supportedProducts)
	log.Info("EMLy is a supported product, distributed by " + productName +
		", which also acts as its monitoring and management agent")
}

func main() {
	// A panic on the main goroutine - the service's SCM dispatcher, the
	// tray's UI thread, a foreground run - shows the fatal-error box first.
	defer crash.Guard(nil)

	inService, err := svc.IsWindowsService()
	if err != nil {
		fatalf("failed to determine session type: %v", err)
	}
	if inService {
		runService()
		return
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "install":
		err = cmdInstall()
	case "uninstall":
		err = cmdUninstall()
	case "start":
		err = cmdStart()
	case "stop":
		err = cmdStop()
	case "run":
		err = cmdRun()
	case "show-toast":
		err = cmdShowToast(os.Args[2:])
	case "show-progress":
		err = cmdShowProgress(os.Args[2:])
	case "products":
		err = cmdProducts(os.Args[2:])
	case "tray":
		// The tray has no console: a fatal error would only make the icon
		// vanish, so it is reported with the box too.
		if err = tray.Run(); err != nil {
			crash.Report(err.Error())
		}
	case "apply-settings":
		err = cmdApplySettings(os.Args[2:])
	case "simulate-crash":
		err = cmdSimulateCrash(os.Args[2:])
	case "restart-service":
		// Internal: launched detached by the service itself for the client
		// channel's service.restart command. cmdStop waits for the service
		// to finish stopping (60s), then cmdStart brings it back.
		err = cmdRestartService()
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatalf("%s failed: %v", os.Args[1], err)
	}
}

// cmdSimulateCrash fails on purpose, the way a real fatal error would, so the
// fatal-error box can be checked without breaking anything:
//
//	panic      a panic on the main goroutine (main's deferred crash.Guard)
//	goroutine  a panic on a goroutine that defers crash.Guard, like the
//	           service's and the tray's
//	error      a fatal error, reported with crash.Report before exiting 1
func cmdSimulateCrash(args []string) error {
	kind := "panic"
	if len(args) > 0 {
		kind = args[0]
	}
	switch kind {
	case "panic":
		panic("crash simulato (simulate-crash panic)")
	case "goroutine":
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer crash.Guard(nil)
			panic("crash simulato in una goroutine (simulate-crash goroutine)")
		}()
		<-done // never reached: the re-panic ends the process
		return nil
	case "error":
		crash.Report("errore simulato (simulate-crash error)")
		os.Exit(1)
		return nil
	default:
		return fmt.Errorf("unknown crash kind %q: use panic, goroutine or error", kind)
	}
}

func usage() {
	_, err := fmt.Fprintf(os.Stderr, "usage: %s install|uninstall|start|stop|run|products [--check]|tray\n", os.Args[0])
	if err != nil {
		return
	}
}

// cmdShowToast displays the update-complete notification in the caller's
// own desktop session. Not meant to be invoked directly - the SYSTEM
// service launches it inside the console user's session via
// internal/notify.LaunchToast, since session 0 has no desktop to draw on.
func cmdShowToast(args []string) error {
	fs := flag.NewFlagSet("show-toast", flag.ContinueOnError)
	exe := fs.String("exe", "", "path to EMLy.exe (icon source)")
	title := fs.String("title", "", "toast title")
	body := fs.String("body", "", "toast body")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return toast.Show(*exe, *title, *body)
}

// cmdShowProgress shows the update progress window until the service, on
// the other end of stdin, closes it. Not meant to be invoked directly - see
// internal/progresswin.
func cmdShowProgress(args []string) error {
	fs := flag.NewFlagSet("show-progress", flag.ContinueOnError)
	title := fs.String("title", productName, "window title")
	icon := fs.String("icon", "", "executable whose icon the window shows")
	waitService := fs.String("wait-service", "", "on EOF, stay up until this service runs under a new process")
	waitPID := fs.Uint("wait-pid", 0, "the service's process ID when the window was opened")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return progresswin.Run(progresswin.Options{
		Title:       *title,
		IconPath:    *icon,
		WaitService: *waitService,
		WaitPID:     uint32(*waitPID),
	}, os.Stdin)
}

// restartServiceStartRetries/Delay bound cmdRestartService's cmdStart
// attempts: the SCM can still be finishing the bookkeeping of the stop this
// same process just performed when the first start is attempted, so one
// failed attempt is expected, not fatal.
const (
	restartServiceStartRetries = 3
	restartServiceStartDelay   = 5 * time.Second
)

// cmdRestartService is not meant to be invoked directly - the running
// service launches it detached (DETACHED_PROCESS, never waited on) for the
// client channel's service.restart command, then closes its own
// WebSocket connection and returns. cmdStop's 60s wait for the service to
// actually stop is why this has to be a separate process rather than
// something the service does to itself: the stop handler blocks the
// service's own goroutine until RunLoop returns.
//
// Nothing else observes this process - no console, no caller waiting on its
// exit code - so it logs to the normal ProgramData log and Event Log itself,
// the same way runService does, rather than leaving a failure with no
// trace. cmdStart is retried a few times with a short pause: it can race the
// SCM's own bookkeeping of the stop this same process just performed.
func cmdRestartService() error {
	_ = config.EnsureDirs() // best-effort: if this fails, so will everything below
	log := logging.New(config.LogsDir(), config.ExeLogPath(), false)
	log.AttachEventLog()
	defer log.Close()

	log.Info("restart-service: stopping the service")
	if err := cmdStop(); err != nil {
		log.ErrorEvent(logging.EventGeneric, "restart-service: stop failed", "error", err.Error())
		return err
	}

	var err error
	for attempt := 1; attempt <= restartServiceStartRetries; attempt++ {
		if err = cmdStart(); err == nil {
			log.Info("restart-service: service restarted", "attempt", attempt)
			return nil
		}
		log.Warn("restart-service: start attempt failed", "attempt", attempt, "error", err.Error())
		if attempt < restartServiceStartRetries {
			time.Sleep(restartServiceStartDelay)
		}
	}
	log.ErrorEvent(logging.EventGeneric, "restart-service: service did not start after retries",
		"attempts", restartServiceStartRetries, "error", err.Error())
	return err
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// runService is the SCM entry point.
func runService() {
	if err := config.EnsureDirs(); err != nil {
		// No logger yet; the SCM records the non-zero exit.
		crash.Report(err.Error())
		os.Exit(1)
	}

	log := logging.New(config.LogsDir(), config.ExeLogPath(), false)
	log.AttachEventLog()
	defer log.Close()

	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		log.ErrorEvent(logging.EventGeneric, "invalid configuration, service cannot start", "error", err.Error())
		crash.Report("configurazione non valida: " + err.Error())
		log.Close() // os.Exit skips the deferred Close
		os.Exit(1)
	}

	logIdentity(log, "service")
	handler := &service.Handler{Updater: service.New(cfg, log, false)}
	if err := svc.Run(service.Name, handler); err != nil {
		log.ErrorEvent(logging.EventGeneric, "service run failed", "error", err.Error())
		crash.Report(err.Error())
		log.Close()
		os.Exit(1)
	}
	log.Info(productName + " service stopped")
}

// cmdRun executes the update loop in the foreground with console logging -
// the debug path; Ctrl+C stops it cleanly.
func cmdRun() error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}

	release, err := acquireSingleton()
	if err != nil {
		return err
	}
	defer release()

	log := logging.New(config.LogsDir(), config.ExeLogPath(), true)
	log.AttachEventLog() // best-effort: works only after `install` registered the source
	defer log.Close()

	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Println(productName + " running in foreground, Ctrl+C to stop")
	logIdentity(log, "foreground run")
	service.New(cfg, log, true).RunLoop(ctx)
	return nil
}

// acquireSingleton guards against a foreground `run` racing the installed
// service (the SCM already prevents two service instances). The raw
// CreateMutexW call is used because x/sys' wrapper drops ERROR_ALREADY_EXISTS
// on success.
func acquireSingleton() (func(), error) {
	name, err := windows.UTF16PtrFromString(`Global\EMLyUpdaterSingleton`)
	if err != nil {
		return nil, err
	}
	createMutex := windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")
	h, _, callErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return nil, fmt.Errorf("CreateMutex failed: %v", callErr)
	}
	if callErr == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(windows.Handle(h))
		return nil, fmt.Errorf("another AryxD Agent (EMLyUpdater) instance is already running (service or foreground)")
	}
	return func() { windows.CloseHandle(windows.Handle(h)) }, nil
}

// cmdInstall registers the auto-start LocalSystem service with restart-on-
// failure recovery, registers the Event Log source, and seeds ProgramData
// with the default config. Idempotent so the updater's installer can re-run it.
// installCertificate puts the 3gIT code-signing certificate into the machine
// trust stores during `install`, so the very first EMLy setup this updater
// runs already elevates as a verified publisher instead of waiting for the
// first poll cycle.
//
// Best-effort and non-fatal: it reports what it did and never blocks service
// registration. Only the machine stores are touched here - `install` runs from
// an installer, where there may be no console user, and the service re-checks
// the per-user stores every cycle anyway.
func installCertificate() {
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		fmt.Printf("note: certificate install skipped, config unreadable: %v\n", err)
		return
	}
	// The bootstrap value, not the remote policy: this runs from the
	// installer, before any service start, so there may be no cached
	// document yet - and the service re-checks the stores every cycle
	// against the effective policy anyway.
	if !cfg.CertificateEnabled {
		return
	}

	_, der, err := cert.Embedded()
	if err != nil {
		fmt.Printf("note: certificate install skipped: %v\n", err)
		return
	}

	installed, err := cert.Ensure(der, cert.MachineTargets(), func(format string, args ...any) {
		fmt.Printf("  %s\n", fmt.Sprintf(format, args...))
	})
	if err != nil {
		fmt.Printf("note: certificate install incomplete: %v\n", err)
		return
	}
	if len(installed) == 0 {
		fmt.Println("code-signing certificate already present in machine trust stores")
	}
}

func cmdInstall() error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}
	// Reset rather than merge: every install (including a self-update) rewrites
	// config.ini from this release's embedded defaults, with the previous file
	// kept as config.prev.ini.
	if changed, err := config.Reset(config.ConfigPath()); err != nil {
		return err
	} else if changed {
		fmt.Printf("config at %s reset to this release's defaults (previous kept as %s)\n",
			config.ConfigPath(), config.BackupPath())
	}

	installCertificate()

	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager (run as administrator?): %w", err)
	}
	defer m.Disconnect()

	svcCfg := mgr.Config{
		StartType: mgr.StartAutomatic,
		// Delayed auto-start plus these two dependencies keep the service out
		// of the earliest part of boot, where it used to start seconds before
		// the network was usable and have its DsGetDcName call fail with "the
		// specified domain either does not exist or could not be contacted" -
		// which the startup source policy reads as "off-site" and pins the
		// machine to the external manifest for the rest of the run.
		//
		// Netlogon is deliberately *not* a dependency: it is Manual on a
		// machine that is not domain-joined, and a dependency the SCM cannot
		// start would stop the updater from starting at all there. Dnscache
		// and LanmanWorkstation are Automatic on every Windows install, and
		// the retry in internal/service/sourcepolicy.go covers whatever slack
		// is left (netlogon, DHCP, Wi-Fi associating after logon).
		DelayedAutoStart: true,
		Dependencies:     []string{"Dnscache", "LanmanWorkstation"},
		DisplayName:      displayName,
		Description:      description,
		ErrorControl:     mgr.ErrorNormal,
		// ServiceStartName left empty = LocalSystem
	}

	s, err := m.OpenService(service.Name)
	if err == nil {
		// Already registered (re-install/upgrade): refresh the configuration
		// instead of failing, so the updater's own installer can always run
		// `install` unconditionally.
		defer s.Close()
		cur, err := s.Config()
		if err != nil {
			return err
		}
		cur.StartType = svcCfg.StartType
		cur.DelayedAutoStart = svcCfg.DelayedAutoStart
		cur.Dependencies = svcCfg.Dependencies
		cur.DisplayName = svcCfg.DisplayName
		cur.Description = svcCfg.Description
		cur.BinaryPathName = exePath
		if err := s.UpdateConfig(cur); err != nil {
			return fmt.Errorf("failed to update existing service config: %w", err)
		}
		fmt.Printf("service %s already registered, configuration refreshed\n", service.Name)
	} else {
		s, err = m.CreateService(service.Name, exePath, svcCfg)
		if err != nil {
			return fmt.Errorf("CreateService failed: %w", err)
		}
		defer s.Close()
		fmt.Printf("service %s installed (%s)\n", service.Name, exePath)
	}

	// Restart automatically on crashes: three restarts 60s apart, counter
	// resets after a day.
	recovery := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}
	if err := s.SetRecoveryActions(recovery, 86400); err != nil {
		return fmt.Errorf("failed to set recovery actions: %w", err)
	}

	if err := eventlog.InstallAsEventCreate(service.Name,
		eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
		// Re-installs hit "already exists" - that is fine.
		fmt.Printf("note: event log source not (re)registered: %v\n", err)
	}
	return nil
}

// cmdUninstall stops and deletes the service and removes the Event Log
// source. ProgramData (config, state, logs, downloads) is deliberately left
// behind so a later re-install resumes where it left off.
// The local log file (next to the exe) is copied to the ProgramData logs
// directory before the InnoSetup uninstaller can delete it.
func cmdUninstall() error {
	// Preserve the exe-dir log to ProgramData before InnoSetup can delete it.
	src := config.ExeLogPath()
	if data, err := os.ReadFile(src); err == nil {
		_ = os.MkdirAll(config.LogsDir(), 0755)
		dst := filepath.Join(config.LogsDir(), "updater-final.log")
		if wErr := os.WriteFile(dst, data, 0644); wErr == nil {
			fmt.Printf("log saved to %s\n", dst)
		}
	}

	_ = cmdStop() // best-effort; service may not be running

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager (run as administrator?): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(service.Name)
	if err != nil {
		return fmt.Errorf("service %s is not installed", service.Name)
	}
	defer s.Close()

	if err := s.Delete(); err != nil {
		return fmt.Errorf("failed to delete service: %w", err)
	}
	if err := eventlog.Remove(service.Name); err != nil {
		fmt.Printf("note: event log source not removed: %v\n", err)
	}
	fmt.Printf("service %s uninstalled (state in %s kept)\n", service.Name, config.DataDir())
	return nil
}

func cmdStart() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager (run as administrator?): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(service.Name)
	if err != nil {
		return fmt.Errorf("service %s is not installed", service.Name)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		return fmt.Errorf("failed to start service: %w", err)
	}
	fmt.Printf("service %s started\n", service.Name)
	return nil
}

func cmdStop() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot connect to service manager (run as administrator?): %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(service.Name)
	if err != nil {
		return fmt.Errorf("service %s is not installed", service.Name)
	}
	defer s.Close()

	status, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("failed to send stop control: %w", err)
	}

	// The stop handler may be in the middle of an install; give it time.
	deadline := time.Now().Add(60 * time.Second)
	for status.State != svc.Stopped {
		if time.Now().After(deadline) {
			return fmt.Errorf("service did not stop within 60s (state %d)", status.State)
		}
		time.Sleep(500 * time.Millisecond)
		status, err = s.Query()
		if err != nil {
			return err
		}
	}
	fmt.Printf("service %s stopped\n", service.Name)
	return nil
}

// cmdProducts prints what the agent knows about each product on this
// machine. Read-only, so it runs beside an installed service (no singleton
// mutex); its own log goes to a temp directory, never to the service's.
func cmdProducts(args []string) error {
	fs := flag.NewFlagSet("products", flag.ContinueOnError)
	check := fs.Bool("check", false, "also ask the server which version each manifest offers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		return err
	}
	log := logging.New(filepath.Join(os.TempDir(), "emly-updater-products"), "", false)
	u := service.New(cfg, log, false)
	return u.ReportProducts(context.Background(), os.Stdout, *check)
}


// cmdApplySettings writes the given config.ini keys and restarts the
// service so it loads them. Run elevated by the tray's settings window
// (internal/tray), through the UAC prompt: config.ini is writable only by
// administrators and SYSTEM. Only config.EditableKeys are accepted, and the
// edited file is validated exactly as the service loads it before it
// replaces the old one - an invalid edit leaves config.ini untouched.
//
// --result names a file the outcome is written to ("ok" or the error), since
// the elevated process has no console the tray could read.
func cmdApplySettings(args []string) (err error) {
	fs := flag.NewFlagSet("apply-settings", flag.ContinueOnError)
	result := fs.String("result", "", "file to write the outcome to")
	restart := fs.Bool("restart", true, "restart the service when it is running")
	if err := fs.Parse(args); err != nil {
		return err
	}
	defer func() {
		if *result == "" {
			return
		}
		msg := "ok"
		if err != nil {
			msg = err.Error()
		}
		_ = os.WriteFile(*result, []byte(msg), 0644)
	}()

	var edits []config.Edit
	for _, a := range fs.Args() {
		e, err := config.ParseEdit(a)
		if err != nil {
			return err
		}
		edits = append(edits, e)
	}
	if len(edits) == 0 {
		return fmt.Errorf("no settings to apply")
	}

	_ = config.EnsureDirs()
	log := logging.New(config.LogsDir(), config.ExeLogPath(), false)
	log.AttachEventLog()
	defer log.Close()

	changes := make([]string, len(edits))
	for i, e := range edits {
		changes[i] = e.Key + "=" + e.Value
	}
	if err := config.WriteEdits(config.ConfigPath(), edits); err != nil {
		log.Warn("apply-settings: config.ini not changed", "changes", strings.Join(changes, " "), "error", err.Error())
		return err
	}
	log.InfoEvent(logging.EventGeneric, "apply-settings: config.ini edited from the tray",
		"changes", strings.Join(changes, " "), "user", currentUser())

	if !*restart || !serviceRunning() {
		return nil
	}
	if err := cmdStop(); err != nil {
		return fmt.Errorf("settings saved, but the service did not stop: %w", err)
	}
	if err := cmdStart(); err != nil {
		return fmt.Errorf("settings saved, but the service did not start again: %w", err)
	}
	return nil
}

// serviceRunning reports whether the agent service is running; false when it
// is not installed or cannot be queried.
func serviceRunning() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()
	s, err := m.OpenService(service.Name)
	if err != nil {
		return false
	}
	defer s.Close()
	st, err := s.Query()
	return err == nil && st.State == svc.Running
}

// currentUser names the account apply-settings runs as, for the log line.
func currentUser() string {
	t := windows.GetCurrentProcessToken()
	u, err := t.GetTokenUser()
	if err != nil {
		return ""
	}
	account, domain, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return u.User.Sid.String()
	}
	return domain + `\` + account
}