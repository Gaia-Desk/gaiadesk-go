package gaiadesk

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Shell is the shell a command runs under.
type Shell string

// The shells. ShellDefault is the desk's own (login shell on macOS/Linux,
// cmd.exe on Windows); ShellNone runs the first argument as the program.
// ShellPowerShell is the same as ShellPwsh (sent as `pwsh`).
const (
	ShellDefault    Shell = "default"
	ShellNone       Shell = "none"
	ShellSh         Shell = "sh"
	ShellBash       Shell = "bash"
	ShellZsh        Shell = "zsh"
	ShellCmd        Shell = "cmd"
	ShellPwsh       Shell = "pwsh"
	ShellPowerShell Shell = "powershell"
)

var execShells = map[Shell]bool{ShellDefault: true, ShellNone: true, ShellSh: true, ShellBash: true, ShellZsh: true, ShellCmd: true, ShellPwsh: true, ShellPowerShell: true}

// A job is a command line: no `none`, and no shell is the desk's default.
var jobShells = map[Shell]bool{ShellSh: true, ShellBash: true, ShellZsh: true, ShellCmd: true, ShellPwsh: true, ShellPowerShell: true}

// wireShell is the shell's name as sent: `powershell` is `pwsh`.
func wireShell(s Shell) string {
	if s == ShellPowerShell {
		return string(ShellPwsh)
	}
	return string(s)
}

// checkDesk checks a desk id: one token, no whitespace, not a flag.
func checkDesk(desk string) (string, error) {
	d := strings.TrimSpace(desk)
	if d == "" {
		return "", usageError("a desk id is required")
	}
	if strings.ContainsAny(d, " \t\r\n\v\f") || strings.HasPrefix(d, "-") {
		return "", usageError("not a desk id: %q", desk)
	}
	return d, nil
}

var jobNameRE = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)

// checkJobName checks a job name: letters, digits, . _ - (not starting with -).
func checkJobName(name string) error {
	if !jobNameRE.MatchString(name) {
		return usageError("a job name is letters, digits, . _ - (not starting with -): %q", name)
	}
	return nil
}

// checkCwd checks a directory on the desk.
func checkCwd(cwd string) error {
	if strings.TrimSpace(cwd) == "" || strings.Contains(cwd, "\x00") {
		return usageError("cwd is a directory on the desk: %q", cwd)
	}
	return nil
}

// checkEnv checks environment variables: a name is non-empty, without `=`,
// whitespace or NUL; a value has no NUL. Errors name the variable, never
// its value.
func checkEnv(env map[string]string) error {
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "= \t\r\n\v\f\x00") {
			return usageError("env: %q is not an environment variable name", k)
		}
		if strings.Contains(v, "\x00") {
			return usageError("env: the value of %s contains a NUL byte", k)
		}
	}
	return nil
}

// wholeSeconds is a duration as whole seconds, rounded up.
func wholeSeconds(d time.Duration) int64 {
	return int64(math.Ceil(d.Seconds()))
}

var (
	durationRE = regexp.MustCompile(`(?i)^\s*\d+\s*[a-z]*(\s*\d+\s*[a-z]+)*\s*$`)
	partRE     = regexp.MustCompile(`(?i)(\d+)\s*([a-z]+)`)
	units      = map[string]int64{"s": 1, "sec": 1, "secs": 1, "m": 60, "min": 60, "mins": 60, "h": 3600, "d": 86400, "w": 604800}
)

// ParseDuration reads a duration as gaiadesk-cli writes them: `90` (seconds),
// `30s`, `10m`, `1h30m`, `7d`, `2w`. Days and weeks are 24 and 168 hours.
func ParseDuration(s string) (time.Duration, error) {
	if !durationRE.MatchString(s) {
		return 0, usageError("not a duration: %q", s)
	}
	t := strings.TrimSpace(s)
	if n, err := strconv.ParseInt(t, 10, 64); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	var total int64
	for _, m := range partRE.FindAllStringSubmatch(t, -1) {
		u, ok := units[strings.ToLower(m[2])]
		if !ok {
			return 0, usageError("unknown unit in %q", s)
		}
		n, _ := strconv.ParseInt(m[1], 10, 64)
		total += n * u
	}
	return time.Duration(total) * time.Second, nil
}
