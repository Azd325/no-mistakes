package daemon

import (
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Uninstall stops this instance's managed daemon and removes its service
// definition. It returns the removed file path (or Windows task name), or an
// empty string when no managed service is installed. Application data is kept.
func Uninstall(p *paths.Paths) (string, error) {
	if serviceManagerBypassed() {
		return "", nil
	}
	var definition string
	var stop func(*paths.Paths) error
	var remove func(*paths.Paths) error
	if runtimeGOOS == "darwin" || runtimeGOOS == "linux" {
		if _, err := serviceUserHomeDir(); err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
	}
	switch runtimeGOOS {
	case "darwin":
		definition, remove = launchAgentPath(p), removeLaunchAgent
		stop = stopLaunchAgent
	case "linux":
		definition, remove = systemdUserServicePath(p), removeSystemdUserService
		stop = stopSystemdUserService
	case "windows":
		definition, remove = windowsTaskName(p), removeWindowsTask
		stop = stopWindowsTask
		output, err := serviceCommandRunner("schtasks", "/Query", "/TN", definition)
		if err != nil {
			if strings.Contains(strings.ToLower(string(output)+" "+err.Error()), "the system cannot find the file specified") {
				return "", nil
			}
			return "", fmt.Errorf("inspect scheduled task %s: %w", definition, err)
		}
	default:
		return "", nil
	}
	if runtimeGOOS != "windows" {
		if _, err := os.Stat(definition); err != nil {
			if os.IsNotExist(err) {
				return "", nil
			}
			return "", fmt.Errorf("inspect managed service %s: %w", definition, err)
		}
	}
	instance := captureRunningDaemon(p)
	if err := stop(p); err != nil {
		return "", fmt.Errorf("stop managed service: %w", err)
	}
	if err := waitForDaemonStop(p, instance); err != nil {
		return "", err
	}
	if err := remove(p); err != nil {
		return "", fmt.Errorf("remove managed service %s: %w", definition, err)
	}
	return definition, nil
}

// InstalledLaunchAgentPath returns the plist left by daemon stop on macOS.
func InstalledLaunchAgentPath(p *paths.Paths) string {
	if runtimeGOOS == "darwin" && managedServiceInstalled(p) {
		return launchAgentPath(p)
	}
	return ""
}
