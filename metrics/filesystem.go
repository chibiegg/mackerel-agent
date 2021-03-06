// +build !windows

package metrics

import (
	"regexp"
	"strings"

	"github.com/mackerelio/mackerel-agent/util"
)

// FilesystemGenerator is common filesystem metrics generator on unix os.
type FilesystemGenerator struct {
	IgnoreRegexp *regexp.Regexp
}

var sanitizerReg = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Generate the metrics of filesystems
func (g *FilesystemGenerator) Generate() (Values, error) {
	filesystems, err := util.CollectDfValues()
	if err != nil {
		return nil, err
	}
	ret := Values{}
	for _, dfs := range filesystems {
		name := dfs.Name
		// https://github.com/docker/docker/blob/v1.5.0/daemon/graphdriver/devmapper/deviceset.go#L981
		if strings.HasPrefix(name, "/dev/mapper/docker-") ||
			(g.IgnoreRegexp != nil && g.IgnoreRegexp.MatchString(name)) {
			continue
		}
		if device := strings.TrimPrefix(name, "/dev/"); name != device {
			device = sanitizerReg.ReplaceAllString(device, "_")
			// kilo bytes -> bytes
			ret["filesystem."+device+".size"] = float64(dfs.Blocks) * 1024
			ret["filesystem."+device+".used"] = float64(dfs.Used) * 1024
		}
	}
	return ret, nil
}
