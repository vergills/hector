package version

import (
	"os/exec"
	"runtime/debug"
	"strings"
)

type Info struct {
	Revision  string
	Modified  bool
	Time      string
	GoVersion string
	Subject   string
	Body      string
}

func Current() Info {
	info := Info{
		Revision:  "unknown",
		Time:      "unknown",
		GoVersion: "unknown",
		Subject:   "unknown",
		Body:      "unknown",
	}
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	info.GoVersion = buildInfo.GoVersion
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Revision = setting.Value
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		case "vcs.time":
			info.Time = setting.Value
		}
	}

	if output, err := exec.Command("git", "show", "-s", "--format=%s%n%b", "HEAD").Output(); err == nil {
		lines := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)
		if len(lines) > 0 && lines[0] != "" {
			info.Subject = lines[0]
		}
		if len(lines) == 2 && strings.TrimSpace(lines[1]) != "" {
			info.Body = strings.TrimSpace(lines[1])
		} else {
			info.Body = "(no extended description)"
		}
	}
	return info
}
