package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	cliconfig "github.com/antonkrylov/xrunner/internal/cli/config"
)

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Print local diagnostic information for troubleshooting",
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, _ := os.Executable()
			exe = strings.TrimSpace(exe)
			look, _ := exec.LookPath("xrunner")
			look = strings.TrimSpace(look)

			fmt.Fprintf(os.Stdout, "xrunner_executable=%s\n", exe)
			if look != "" {
				fmt.Fprintf(os.Stdout, "xrunner_on_path=%s\n", look)
			}
			if exe != "" && look != "" {
				absExe, _ := filepath.EvalSymlinks(exe)
				absLook, _ := filepath.EvalSymlinks(look)
				if absExe != "" && absLook != "" && absExe != absLook {
					fmt.Fprintln(os.Stdout, "warning=you_are_not_running_the_same_xrunner_as_on_PATH (adjust PATH or call the intended binary explicitly)")
				}
			}
			fmt.Fprintf(os.Stdout, "PATH=%s\n", os.Getenv("PATH"))

			home, _ := os.UserHomeDir()
			if home != "" {
				userBin := filepath.Join(home, ".local", "bin")
				inPath := false
				for _, p := range filepath.SplitList(os.Getenv("PATH")) {
					if filepath.Clean(p) == filepath.Clean(userBin) {
						inPath = true
						break
					}
				}
				fmt.Fprintf(os.Stdout, "user_bin=%s\n", userBin)
				fmt.Fprintf(os.Stdout, "user_bin_in_PATH=%t\n", inPath)
			}

			cfgPath := effectiveConfigPath(cmd)
			fmt.Fprintf(os.Stdout, "config_path=%s\n", cfgPath)
			cfg, err := cliconfig.Load(cfgPath)
			if err != nil {
				fmt.Fprintf(os.Stdout, "config_error=%s\n", err.Error())
				return nil
			}
			if cfg == nil {
				fmt.Fprintln(os.Stdout, "config_present=false")
				return nil
			}
			fmt.Fprintln(os.Stdout, "config_present=true")
			fmt.Fprintf(os.Stdout, "current_context=%s\n", strings.TrimSpace(cfg.CurrentContext))
			names := make([]string, 0, len(cfg.Contexts))
			for k := range cfg.Contexts {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, name := range names {
				c := cfg.Contexts[name]
				if c == nil {
					continue
				}
				fmt.Fprintf(os.Stdout, "context=%s api=%s ssh=%s timeout=%d\n",
					name,
					strings.TrimSpace(c.Server),
					strings.TrimSpace(c.SSHHost),
					c.TimeoutSeconds,
				)
			}
			return nil
		},
	}
	return cmd
}
