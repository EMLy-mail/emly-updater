// EMLyUpdater is a standalone Windows service (LocalSystem, auto-start) that
// keeps EMLy up to date on domain-joined machines: it polls an update
// manifest (HTTP primary, UNC fallback), downloads and SHA256-verifies the
// InnoSetup installer, and applies it silently — immediately when EMLy is
// closed, on exit when it is open, or force-killing it for critical updates.
//
// Subcommands:
//
//	install    register the auto-start service + Event Log source (admin)
//	uninstall  stop and remove the service (ProgramData state is kept)
//	start      start the service
//	stop       stop the service
//	run        run the update loop in the foreground (debug)
//
// Without arguments the binary expects to be launched by the SCM.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/service"
)

const (
	displayName = "EMLy Updater Service"
	description = "Keeps the EMLy email viewer up to date: polls the update manifest, verifies and installs new releases silently."
)

func main() {
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
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fatalf("%s failed: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s install|uninstall|start|stop|run\n", os.Args[0])
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// runService is the SCM entry point.
func runService() {
	if err := config.EnsureDirs(); err != nil {
		// No logger yet; the SCM records the non-zero exit.
		os.Exit(1)
	}

	log := logging.New(config.LogsDir(), config.ExeLogPath(), false)
	log.AttachEventLog()
	defer log.Close()

	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		log.ErrorEvent(logging.EventGeneric, "invalid configuration, service cannot start", "error", err.Error())
		os.Exit(1)
	}

	log.Info("EMLyUpdater service starting")
	handler := &service.Handler{Updater: service.New(cfg, log)}
	if err := svc.Run(service.Name, handler); err != nil {
		log.ErrorEvent(logging.EventGeneric, "service run failed", "error", err.Error())
		os.Exit(1)
	}
	log.Info("EMLyUpdater service stopped")
}

// cmdRun executes the update loop in the foreground with console logging —
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

	fmt.Println("EMLyUpdater running in foreground, Ctrl+C to stop")
	service.New(cfg, log).RunLoop(ctx)
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
		return nil, fmt.Errorf("another EMLyUpdater instance is already running (service or foreground)")
	}
	return func() { windows.CloseHandle(windows.Handle(h)) }, nil
}

// cmdInstall registers the auto-start LocalSystem service with restart-on-
// failure recovery, registers the Event Log source, and seeds ProgramData
// with the default config. Idempotent so the updater's installer can re-run it.
func cmdInstall() error {
	if err := config.EnsureDirs(); err != nil {
		return err
	}
	if created, err := config.WriteDefault(config.ConfigPath()); err != nil {
		return err
	} else if created {
		fmt.Printf("wrote default config to %s\n", config.ConfigPath())
	}

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
		StartType:    mgr.StartAutomatic,
		DisplayName:  displayName,
		Description:  description,
		ErrorControl: mgr.ErrorNormal,
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
		// Re-installs hit "already exists" — that is fine.
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
