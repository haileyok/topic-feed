// Command labelreport writes the review report and viewer page for a labeling run:
//
//	labelreport -taxonomy taxonomy/v1.yaml -label-config <hash> [-out dir]
//
// The default output directory is /data/reports/<version>-<label_config>, which the
// viewer service serves.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelreport"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func main() {
	taxPath := flag.String("taxonomy", "taxonomy/v1.yaml", "taxonomy YAML file")
	labelConfig := flag.String("label-config", "", "label_config hash of the run (printed by labeler)")
	out := flag.String("out", "", "output directory (default /data/reports/<version>-<label_config>)")
	perTopic := flag.Int("examples", 10, "examples per broad topic")
	flag.Parse()
	if err := run(*taxPath, *labelConfig, *out, *perTopic); err != nil {
		fmt.Fprintln(os.Stderr, "labelreport:", err)
		os.Exit(1)
	}
}

func run(taxPath, labelConfig, out string, perTopic int) error {
	if labelConfig == "" {
		return fmt.Errorf("-label-config is required")
	}
	tax, err := taxonomy.Load(taxPath)
	if err != nil {
		return err
	}
	if out == "" {
		out = labelreport.DefaultDir(tax, labelConfig)
	}
	ctx := context.Background()
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()
	return labelreport.Write(ctx, conn, tax, labelConfig, out, perTopic)
}
