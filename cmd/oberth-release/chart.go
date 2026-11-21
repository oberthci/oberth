package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v3/pkg/chart/loader"
)

func chart(o options, images map[string]string) error {
	files, err := chartFiles(o.chart)
	if err != nil {
		return err
	}
	files["Chart.yaml"], err = stampLines(files["Chart.yaml"], map[string]string{"version": "0.0.0", "appVersion": "unset"}, map[string]string{"version": strings.TrimPrefix(o.version, "v"), "appVersion": o.version})
	if err != nil {
		return err
	}
	files["values.yaml"], err = stampValues(files["values.yaml"], images)
	if err != nil {
		return err
	}
	return packageChart(o, files)
}
func stampValues(values []byte, images map[string]string) ([]byte, error) {
	return stampLines(values, map[string]string{"imageDigest": "unset", "sourcePluginImageDigest": "unset"}, map[string]string{"imageDigest": images["oberth"], "sourcePluginImageDigest": images["oberth-source-plugin"]})
}
func stampLines(body []byte, old, values map[string]string) ([]byte, error) {
	lines := bytes.SplitAfter(body, []byte("\n"))
	for key, value := range values {
		count := 0
		for i, line := range lines {
			if bytes.Equal(line, []byte(key+": "+old[key]+"\n")) {
				count++
				lines[i] = []byte(key + ": " + value + "\n")
			}
		}
		if count != 1 {
			return nil, chartStamp
		}
	}
	return bytes.Join(lines, nil), nil
}
func chartFiles(dir string) (map[string][]byte, error) {
	chart, err := loader.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	if chart.Metadata.Name != "oberth" {
		return nil, inputs
	}
	files := make(map[string][]byte, len(chart.Raw))
	for _, file := range chart.Raw {
		files[file.Name] = file.Data
	}
	// Preserve source bytes, including a UTF-8 BOM, after the loader's validation.
	for _, name := range []string{"Chart.yaml", "values.yaml"} {
		if files[name] == nil {
			return nil, inputs
		}
		files[name], err = os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}
func packageChart(o options, files map[string][]byte) error {
	packaged := make(map[string][]byte)
	for name, body := range files {
		packaged["oberth/"+name] = body
	}
	body, err := archive(packaged, false)
	if err != nil {
		return err
	}
	body, err = compressed(body)
	if err != nil {
		return err
	}
	return writeFile(o, "oberth.tgz", body)
}
