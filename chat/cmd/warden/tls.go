package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"warden/chat/internal/transport"
)

// tlsCommand is `warden tls bootstrap`: a deployment CA and one certificate
// per service identity for the tls:// transport (docs/warden-kubernetes-plan.md,
// decision 5), for a cluster without cert-manager (a chart bootstrap Job
// runs it and stores each directory as a TLS Secret) and for the dev loop.
func (c *cli) tls(args []string) error {
	if len(args) == 0 || args[0] != "bootstrap" {
		fmt.Fprint(c.stderr, "usage: warden tls bootstrap --out DIR [--names a,b,c] [--sans host,ip,...] [--days N]\n")
		return errUsage
	}
	fs := flag.NewFlagSet("warden tls bootstrap", flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	out := fs.String("out", "", "directory to write ca.crt, ca.key and one <name>/{ca.crt,tls.crt,tls.key} per identity (required)")
	names := fs.String("names", strings.Join([]string{transport.Policy, transport.Runner, transport.Chat, transport.Edge}, ","), "comma-separated identities to issue certificates for")
	sans := fs.String("sans", "", "comma-separated extra DNS names or IP addresses added to every certificate (e.g. the Service FQDNs, or 127.0.0.1 for a local run)")
	days := fs.Int("days", 365, "certificate validity in days (the CA lasts ten times as long)")
	if err := fs.Parse(args[1:]); err != nil {
		return errUsage
	}
	if *out == "" || *days < 1 {
		fs.Usage()
		return errUsage
	}
	identities := split(*names)
	if len(identities) == 0 {
		return fmt.Errorf("--names must name at least one identity")
	}
	dir, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if _, err = transport.Bootstrap(dir, identities, split(*sans), time.Now(), time.Duration(*days)*24*time.Hour); err != nil {
		return err
	}
	fmt.Fprintf(c.stdout, "CA written to %s (ca.crt, ca.key; keep ca.key private)\n", dir)
	for _, id := range identities {
		fmt.Fprintf(c.stdout, "%s: %s\n", id, filepath.Join(dir, id))
	}
	return nil
}

func split(list string) []string {
	var out []string
	for _, v := range strings.Split(list, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
