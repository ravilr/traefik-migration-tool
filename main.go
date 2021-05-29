package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/traefik/traefik-migration-tool/ingress"
)

var (
	Version     = "dev"
	ShortCommit = ""
	Date        = ""
)

type acmeConfig struct {
	input        string
	output       string
	resolverName string
}

type ingressConfig struct {
	input  string
	output string
}

type staticConfig struct {
	input     string
	outputDir string
}

func main() {
	log.SetFlags(log.Lshortfile)

	rootCmd := &cobra.Command{
		Use:     "traefik-migration-tool",
		Short:   "A tool to migrate from Traefik v1 to Traefik v2.",
		Long:    `A tool to migrate from Traefik v1 to Traefik v2.`,
		Version: Version,
	}

	var ingressCfg ingressConfig

	ingressCmd := &cobra.Command{
		Use:   "ingress",
		Short: "Migrate 'Ingress' to Traefik v1 & v2 compatible resources.",
		Long:  "Migrate 'Ingress' to Traefik v1 & v2 compatible resources.",
		PreRunE: func(_ *cobra.Command, _ []string) error {
			fmt.Printf("Traefik Migration: %s - %s - %s\n", Version, Date, ShortCommit)

			if ingressCfg.input == "" || ingressCfg.output == "" {
				return errors.New("input and output flags are required")
			}

			info, err := os.Stat(ingressCfg.output)
			if err != nil {
				if !os.IsNotExist(err) {
					return err
				}
				err = os.MkdirAll(ingressCfg.output, 0755)
				if err != nil {
					return err
				}
			} else if !info.IsDir() {
				return errors.New("output must be a directory")
			}

			return nil
		},
		RunE: func(_ *cobra.Command, _ []string) error {
			return ingress.Convert(ingressCfg.input, ingressCfg.output)
		},
	}

	ingressCmd.Flags().StringVarP(&ingressCfg.input, "input", "i", "", "Input directory.")
	ingressCmd.Flags().StringVarP(&ingressCfg.output, "output", "o", "./output", "Output directory.")

	rootCmd.AddCommand(ingressCmd)

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Display version",
		Run: func(_ *cobra.Command, _ []string) {
			displayVersion(rootCmd.Name())
		},
	}

	rootCmd.AddCommand(versionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func displayVersion(name string) {
	fmt.Printf(name+`:
 version     : %s
 commit      : %s
 build date  : %s
 go version  : %s
 go compiler : %s
 platform    : %s/%s
`, Version, ShortCommit, Date, runtime.Version(), runtime.Compiler, runtime.GOOS, runtime.GOARCH)
}
