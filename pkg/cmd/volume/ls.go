/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package volume

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"

	"github.com/containerd/containerd/pkg/progress"
	types "github.com/containerd/nerdctl/pkg/api/types"
	"github.com/containerd/nerdctl/pkg/formatter"
	"github.com/containerd/nerdctl/pkg/inspecttypes/native"
	"github.com/sirupsen/logrus"
)

type volumePrintable struct {
	Driver     string
	Labels     string
	Mountpoint string
	Name       string
	Scope      string
	Size       string
	// TODO: "Links"
}

func Ls(options *types.VolumeLsCommandOptions, stdout io.Writer) error {
	if options.Quiet && options.Size {
		logrus.Warn("cannot use --size and --quiet together, ignoring --size")
		options.Size = false
	}
	sizeFilter := hasSizeFilter(options.Filters)
	if sizeFilter && options.Quiet {
		logrus.Warn("cannot use --filter=size and --quiet together, ignoring --filter=size")
		options.Filters = removeSizeFilters(options.Filters)
	}
	if sizeFilter && !options.Size {
		logrus.Warn("should use --filter=size and --size together")
		options.Size = true
	}
	vols, err := List(
		options.GOptions.Namespace,
		options.GOptions.DataRoot,
		options.GOptions.Address,
		options.Size,
		options.Filters,
	)
	if err != nil {
		return err
	}
	return lsPrintOutput(options, stdout, vols)
}

func hasSizeFilter(filters []string) bool {
	for _, filter := range filters {
		if strings.HasPrefix(filter, "size") {
			return true
		}
	}
	return false
}

func removeSizeFilters(filters []string) []string {
	var res []string
	for _, filter := range filters {
		if !strings.HasPrefix(filter, "size") {
			res = append(res, filter)
		}
	}
	return res
}

// List contains the main logic and can be used by projects other than CLIs.
// If improvements are added (e.g., filtering by dangling=true, driver=local, etc.),
// other projects can also benefit from this.
func List(namespace, dataRoot, address string, size bool, filters []string) (map[string]native.Volume, error) {
	labelFilterFuncs, nameFilterFuncs, sizeFilterFuncs, isFilter, err := getVolumeFilterFuncs(filters)
	if err != nil {
		return nil, err
	}
	vols, err := Volumes(namespace, dataRoot, address, size)
	if err != nil {
		return nil, err
	}
	for k, v := range vols {
		if isFilter && !volumeMatchesFilter(v, labelFilterFuncs, nameFilterFuncs, sizeFilterFuncs) {
			delete(vols, k)
		}
	}
	return vols, nil
}

func lsPrintOutput(options *types.VolumeLsCommandOptions, stdout io.Writer, vols map[string]native.Volume) error {
	w := stdout
	var tmpl *template.Template
	switch options.Format {
	case "", "table", "wide":
		w = tabwriter.NewWriter(stdout, 4, 8, 4, ' ', 0)
		if !options.Quiet {
			if options.Size {
				fmt.Fprintln(w, "VOLUME NAME\tDIRECTORY\tSIZE")
			} else {
				fmt.Fprintln(w, "VOLUME NAME\tDIRECTORY")
			}
		}
	case "raw":
		return errors.New("unsupported format: \"raw\"")
	default:
		if options.Quiet {
			return errors.New("format and quiet must not be specified together")
		}
		var err error
		tmpl, err = formatter.ParseTemplate(options.Format)
		if err != nil {
			return err
		}
	}

	for _, v := range vols {
		p := volumePrintable{
			Driver:     "local",
			Labels:     "",
			Mountpoint: v.Mountpoint,
			Name:       v.Name,
			Scope:      "local",
		}
		if v.Labels != nil {
			p.Labels = formatter.FormatLabels(*v.Labels)
		}
		if options.Size {
			p.Size = progress.Bytes(v.Size).String()
		}
		if tmpl != nil {
			var b bytes.Buffer
			if err := tmpl.Execute(&b, p); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, b.String()+"\n"); err != nil {
				return err
			}
		} else if options.Quiet {
			fmt.Fprintln(w, p.Name)
		} else if options.Size {
			fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, p.Mountpoint, p.Size)
		} else {
			fmt.Fprintf(w, "%s\t%s\n", p.Name, p.Mountpoint)
		}
	}
	if f, ok := w.(formatter.Flusher); ok {
		return f.Flush()
	}
	return nil
}

func Volumes(ns string, dataRoot string, address string, volumeSize bool) (map[string]native.Volume, error) {
	volStore, err := Store(ns, dataRoot, address)
	if err != nil {
		return nil, err
	}
	return volStore.List(volumeSize)
}

func getVolumeFilterFuncs(filters []string) ([]func(*map[string]string) bool, []func(string) bool, []func(int64) bool, bool, error) {
	isFilter := len(filters) > 0
	labelFilterFuncs := make([]func(*map[string]string) bool, 0)
	nameFilterFuncs := make([]func(string) bool, 0)
	sizeFilterFuncs := make([]func(int64) bool, 0)
	sizeOperators := []struct {
		Operand string
		Compare func(int64, int64) bool
	}{
		{">=", func(size, volumeSize int64) bool {
			return volumeSize >= size
		}},
		{"<=", func(size, volumeSize int64) bool {
			return volumeSize <= size
		}},
		{">", func(size, volumeSize int64) bool {
			return volumeSize > size
		}},
		{"<", func(size, volumeSize int64) bool {
			return volumeSize < size
		}},
		{"=", func(size, volumeSize int64) bool {
			return volumeSize == size
		}},
	}
	for _, filter := range filters {
		if strings.HasPrefix(filter, "name") || strings.HasPrefix(filter, "label") {
			subs := strings.SplitN(filter, "=", 2)
			if len(subs) < 2 {
				continue
			}
			switch subs[0] {
			case "name":
				nameFilterFuncs = append(nameFilterFuncs, func(name string) bool {
					return strings.Contains(name, subs[1])
				})
			case "label":
				v, k, hasValue := "", subs[1], false
				if subs := strings.SplitN(subs[1], "=", 2); len(subs) == 2 {
					hasValue = true
					k, v = subs[0], subs[1]
				}
				labelFilterFuncs = append(labelFilterFuncs, func(labels *map[string]string) bool {
					if labels == nil {
						return false
					}
					val, ok := (*labels)[k]
					if !ok || (hasValue && val != v) {
						return false
					}
					return true
				})
			}
			continue
		}
		if strings.HasPrefix(filter, "size") {
			for _, sizeOperator := range sizeOperators {
				if subs := strings.SplitN(filter, sizeOperator.Operand, 2); len(subs) == 2 {
					v, err := strconv.Atoi(subs[1])
					if err != nil {
						return nil, nil, nil, false, err
					}
					sizeFilterFuncs = append(sizeFilterFuncs, func(size int64) bool {
						return sizeOperator.Compare(int64(v), size)
					})
					break
				}
			}
			continue
		}
	}
	return labelFilterFuncs, nameFilterFuncs, sizeFilterFuncs, isFilter, nil
}

func volumeMatchesFilter(vol native.Volume, labelFilterFuncs []func(*map[string]string) bool, nameFilterFuncs []func(string) bool, sizeFilterFuncs []func(int64) bool) bool {
	for _, labelFilterFunc := range labelFilterFuncs {
		if !labelFilterFunc(vol.Labels) {
			return false
		}
	}
	for _, nameFilterFunc := range nameFilterFuncs {
		if !nameFilterFunc(vol.Name) {
			return false
		}
	}
	for _, sizeFilterFunc := range sizeFilterFuncs {
		if !sizeFilterFunc(vol.Size) {
			return false
		}
	}
	return true
}
