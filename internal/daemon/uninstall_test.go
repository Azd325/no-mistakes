package daemon

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestUninstallStopsAndRemovesOnlyThisInstancesService(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			defer stubServiceRuntime(t)()
			runtimeGOOS = platform
			home := t.TempDir()
			serviceUserHomeDir = func() (string, error) { return home, nil }
			serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
			p := paths.WithRoot(t.TempDir())
			other := paths.WithRoot(t.TempDir())
			if err := os.WriteFile(filepath.Join(p.Root(), "keep-data"), []byte("data"), 0o600); err != nil {
				t.Fatal(err)
			}
			definition, otherDefinition := windowsTaskName(p), windowsTaskName(other)
			var want []string
			switch platform {
			case "darwin":
				definition, otherDefinition = launchAgentPath(p), launchAgentPath(other)
				want = []string{"launchctl bootout gui/501/" + launchdServiceLabel(p)}
			case "linux":
				definition, otherDefinition = systemdUserServicePath(p), systemdUserServicePath(other)
				want = []string{"systemctl --user stop " + systemdServiceName(p), "systemctl --user disable " + systemdServiceName(p), "systemctl --user daemon-reload"}
			case "windows":
				want = []string{"schtasks /Query /TN " + definition, "schtasks /End /TN " + definition, "schtasks /Delete /TN " + definition + " /F"}
			}
			if platform != "windows" {
				for _, file := range []string{definition, otherDefinition} {
					if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, []byte("service"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			installed := true
			stopped := false
			var commands []string
			serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
				command := name + " " + strings.Join(args, " ")
				commands = append(commands, command)
				if strings.Contains(command, "/Query") && !installed {
					return nil, fmt.Errorf("ERROR: The system cannot find the file specified.")
				}
				if command == want[0] && platform != "windows" || strings.Contains(command, "/End") {
					stopped = true
					if platform != "windows" {
						if _, err := os.Stat(definition); err != nil {
							t.Fatalf("service definition must remain until stopped: %v", err)
						}
					}
				}
				if strings.Contains(command, "/Delete") {
					installed = false
				}
				return nil, nil
			}
			daemonHealthCheck = func(*paths.Paths) (bool, error) {
				if !stopped {
					t.Fatal("must stop the managed service before waiting for exit")
				}
				return false, nil
			}
			got, err := Uninstall(p)
			if err != nil || got != definition {
				t.Fatalf("Uninstall = %q, %v; want %q", got, err, definition)
			}
			if !reflect.DeepEqual(commands, want) {
				t.Fatalf("commands = %v, want %v", commands, want)
			}
			if platform != "windows" {
				if _, err := os.Stat(definition); !os.IsNotExist(err) {
					t.Fatalf("service definition remains: %v", err)
				}
				if _, err := os.Stat(otherDefinition); err != nil {
					t.Fatalf("another instance's service changed: %v", err)
				}
			}
			if data, err := os.ReadFile(filepath.Join(p.Root(), "keep-data")); err != nil || string(data) != "data" {
				t.Fatalf("application data changed: %q, %v", data, err)
			}
			if got, err := Uninstall(p); err != nil || got != "" {
				t.Fatalf("second Uninstall = %q, %v; want no-op", got, err)
			}
		})
	}
}

func TestUninstallWithoutManagedServiceIsANoOp(t *testing.T) {
	for _, platform := range []string{"darwin", "linux", "windows", "freebsd"} {
		t.Run(platform, func(t *testing.T) {
			defer stubServiceRuntime(t)()
			runtimeGOOS = platform
			home := t.TempDir()
			serviceUserHomeDir = func() (string, error) { return home, nil }
			serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
				if platform == "windows" && name == "schtasks" && args[0] == "/Query" {
					return []byte("ERROR: The system cannot find the file specified."), fmt.Errorf("exit status 1")
				}
				t.Fatalf("unexpected service mutation: %s %v", name, args)
				return nil, nil
			}
			p := paths.WithRoot(filepath.Join(t.TempDir(), "absent"))
			if got, err := Uninstall(p); err != nil || got != "" {
				t.Fatalf("Uninstall = %q, %v; want no-op", got, err)
			}
			if _, err := os.Stat(p.Root()); !os.IsNotExist(err) {
				t.Fatalf("uninstall should not create application state: %v", err)
			}
		})
	}
}

func TestUninstallKeepsDefinitionWhenStopFails(t *testing.T) {
	defer stubServiceRuntime(t)()
	runtimeGOOS = "darwin"
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	serviceCurrentUser = func() (*user.User, error) { return &user.User{Uid: "501"}, nil }
	p := paths.WithRoot(t.TempDir())
	file := launchAgentPath(p)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("service"), 0o644); err != nil {
		t.Fatal(err)
	}
	serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("permission denied") }
	if got, err := Uninstall(p); err == nil || got != "" || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Uninstall = %q, %v; want stop failure", got, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("failed stop must keep service definition: %v", err)
	}
}

func TestUninstallWindowsQueryFailureIsNotReportedAsAbsent(t *testing.T) {
	defer stubServiceRuntime(t)()
	runtimeGOOS = "windows"
	serviceCommandRunner = func(string, ...string) ([]byte, error) { return nil, fmt.Errorf("access denied") }
	if got, err := Uninstall(paths.WithRoot(t.TempDir())); err == nil || got != "" || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("Uninstall = %q, %v; want inspection failure", got, err)
	}
}

func TestInstalledLaunchAgentPathNamesOnlyAnInstalledMacOSService(t *testing.T) {
	defer stubServiceRuntime(t)()
	home := t.TempDir()
	serviceUserHomeDir = func() (string, error) { return home, nil }
	p := paths.WithRoot(t.TempDir())
	runtimeGOOS = "darwin"
	if got := InstalledLaunchAgentPath(p); got != "" {
		t.Fatalf("absent service path = %q", got)
	}
	file := launchAgentPath(p)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("service"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := InstalledLaunchAgentPath(p); got != file {
		t.Fatalf("installed service path = %q, want %q", got, file)
	}
	runtimeGOOS = "linux"
	if got := InstalledLaunchAgentPath(p); got != "" {
		t.Fatalf("non-macOS service path = %q", got)
	}
}
