// Copyright 2021-2023 Zenauth Ltd.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	containeranalysis "cloud.google.com/go/containeranalysis/apiv1"
	"github.com/cerbos/reimage"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"

	"k8s.io/apimachinery/pkg/util/yaml"
)

type app struct {
	Version                bool
	RenameIgnore           string
	renameIgnore           *regexp.Regexp
	RenameRemotePath       string
	RenameTemplateString   string
	remoteTemplate         *template.Template
	Clobber                bool
	NoCopy                 bool
	RulesConfigFile        string
	imagFinder             reimage.ImagesFinder
	DryRun                 bool
	WriteMappings          string
	WriteMappingsImg       string
	StaticMappings         string
	StaticMappingsImg      string
	VulnCheckGrafeasParent string
	VulnCheckTimeout       time.Duration
	VulnCheckIgnoreList    []string
	VulnCheckMaxCVSS       float64
	VulnCheckIgnoreImages  string
	vulnCheckIgnoreImages  *regexp.Regexp
	Debug                  bool

	log *slog.Logger
}

func setup() (*app, error) {
	var err error
	a := app{}
	vulnIgnoreStr := ""
	flag.BoolVar(&a.Version, "V", false, "print version/build info")
	flag.BoolVar(&a.DryRun, "dryrun", false, "only log actions")
	flag.BoolVar(&a.Debug, "debug", false, "enable debug logging")

	flag.StringVar(&a.RulesConfigFile, "rules-config", "", "yaml definition of kind/image-path mappings")

	flag.StringVar(&a.RenameIgnore, "rename-ignore", "^$", "ignore images matching this expression")
	flag.StringVar(&a.RenameRemotePath, "rename-remote-path", "", "template for remapping imported images")
	flag.StringVar(&a.RenameTemplateString, "rename-template", reimage.DefaultTemplateStr, "template for remapping imported images")

	flag.BoolVar(&a.Clobber, "clobber", false, "allow overwriting remote images")
	flag.BoolVar(&a.NoCopy, "no-copy", false, "disable copying of renamed images")

	flag.StringVar(&a.WriteMappings, "write-json-mappings-file", "", "write final image mappings to a json file")
	flag.StringVar(&a.WriteMappingsImg, "write-json-mappings-img", "", "write final image mapping to a registry image")
	flag.StringVar(&a.StaticMappings, "static-json-mappings-file", "", "take all mappings from a mappings file")
	flag.StringVar(&a.StaticMappingsImg, "static-json-mappings-img", "", "take all mapping from a mappings registry image")

	flag.StringVar(&a.VulnCheckGrafeasParent, "vulncheck-grafeas-parent", "", "value for the parent of the grafeas client (e.g. \"project/my-project-id\" for GCP")
	flag.DurationVar(&a.VulnCheckTimeout, "vuln-check-timeout", 5*time.Minute, "how long to wait for vulnerability scanning to complete")
	flag.StringVar(&vulnIgnoreStr, "vulncheck-ignore-cve-list", "", "comma separated list of vulnerabilities to ignore")
	flag.Float64Var(&a.VulnCheckMaxCVSS, "vulncheck-max-cvss", 9.0, "maximum CVSS vulnerabitility score")
	flag.StringVar(&a.VulnCheckIgnoreImages, "vulncheck-ignore-images", "", "regexp of images to skip for CVE checks")
	flag.Parse()

	if a.Version {
		printVersion()
		os.Exit(0)
	}

	for _, str := range strings.Split(vulnIgnoreStr, ",") {
		str = strings.TrimSpace(str)
		if str == "" {
			continue
		}
		a.VulnCheckIgnoreList = append(a.VulnCheckIgnoreList, str)
	}

	log := a.setupLog()
	a.log = log

	if a.RenameIgnore != "" {
		a.renameIgnore = regexp.MustCompile(a.RenameIgnore)
	}

	if a.VulnCheckIgnoreImages != "" {
		a.vulnCheckIgnoreImages = regexp.MustCompile(a.VulnCheckIgnoreImages)
	}

	// What follows is horrid, and probably a sign of some abstraction breakdown
	// But basically, if static mapping was specified, we disable/ignore
	// the rename mapping
	if a.StaticMappings != "" || a.StaticMappingsImg != "" {
		if a.StaticMappings != "" && a.StaticMappingsImg != "" {
			return &a, fmt.Errorf("only one static mappings configuration is allowed")
		}
		if a.RenameRemotePath != "" || a.RenameTemplateString != reimage.DefaultTemplateStr {
			log.Info("settings static mappings disables image renaming ")
			a.RenameRemotePath = ""
			a.RenameTemplateString = ""
		}
	}

	if a.RenameRemotePath != "" && a.RenameTemplateString != "" {
		a.remoteTemplate, err = template.New("remote").Parse(a.RenameTemplateString)
		if err != nil {
			return &a, fmt.Errorf("failed parsing remote template, %w", err)
		}
	} else {
		if a.StaticMappings == "" && a.StaticMappingsImg == "" {
			log.Info("copying disabled, (remote path and remote template must be set)")
		}
	}

	err = a.setupRulesConfigs()
	if err != nil {
		return &a, err
	}

	return &a, nil
}

func (a *app) setupRulesConfigs() error {
	var err error
	ruleConfig := []byte{}
	if a.RulesConfigFile != "" {
		ruleConfig, err = os.ReadFile(a.RulesConfigFile)
		if err != nil {
			return fmt.Errorf("failed reading json matcher definitions, %w", err)
		}
	}

	var jmCfgs []reimage.JSONImageFinderConfig
	err = yaml.Unmarshal(ruleConfig, &jmCfgs)
	if err != nil {
		return fmt.Errorf("could not compile json matchers, %w", err)
	}

	jmCfgs = append(jmCfgs, reimage.DefaultRulesConfig...)
	a.imagFinder, err = reimage.CompileJSONImageFinders(jmCfgs)
	if err != nil {
		return fmt.Errorf("could not compile json matchers, %w", err)
	}
	return nil
}

func readStaticMappingsImage(src string) ([]byte, error) {
	rimg, err := crane.Pull(src)
	if err != nil {
		return nil, fmt.Errorf("image pull failed, %w", err)
	}

	lys, err := rimg.Layers()
	if err != nil {
		return nil, fmt.Errorf("could not read image layers, %w", err)
	}
	if len(lys) != 1 {
		return nil, errors.New("multi-layer image, not from reimage")
	}

	lrdr, err := lys[0].Uncompressed()
	if err != nil {
		return nil, fmt.Errorf("could not read image layer, %w", err)
	}

	tarrdr := tar.NewReader(lrdr)
	_, err = tarrdr.Next()
	if err != nil {
		return nil, fmt.Errorf("could not read image layer tar file, %w", err)
	}
	lbs := bytes.NewBuffer([]byte{})
	_, err = io.Copy(lbs, tarrdr)
	if err != nil {
		return nil, fmt.Errorf("failed reading image layer tar content, %w", err)
	}

	return lbs.Bytes(), nil
}

func readStaticMappingsFile(src string) ([]byte, error) {
	return os.ReadFile(src)
}

func (a *app) readStaticMappings() (*reimage.StaticRemapper, error) {
	var bs []byte
	var err error
	switch {
	case a.StaticMappings != "":
		bs, err = readStaticMappingsFile(a.StaticMappings)
	case a.StaticMappingsImg != "":
		bs, err = readStaticMappingsImage(a.StaticMappingsImg)
	default:
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed reading json mappings, %v", err)
	}

	rimgs := map[string]reimage.QualifiedImage{}
	err = json.Unmarshal(bs, &rimgs)
	if err != nil {
		return nil, fmt.Errorf("could not parse as JSON map, %v", err)
	}
	return reimage.NewStaticRemapper(rimgs)
}

func (a *app) writeMappings(mappings map[string]reimage.QualifiedImage) (err error) {
	bs, _ := json.Marshal(mappings)

	if a.DryRun {
		a.log.Info("dry-run, will not write static mappings file")
		return nil
	}

	if a.WriteMappings != "" {
		err = os.WriteFile(a.WriteMappings, bs, 0644)
		if err != nil {
			return fmt.Errorf("could not write file, %w", err)
		}
	}

	if a.WriteMappingsImg != "" {
		cnt := map[string][]byte{
			"reimage-mapping.json": bs,
		}
		img, err := crane.Image(cnt)
		if err != nil {
			return fmt.Errorf("could not create image, %w", err)
		}

		err = crane.Push(img, a.WriteMappingsImg)
		if err != nil {
			return fmt.Errorf("could not push image, %w", err)
		}
	}

	return nil
}

func (a *app) setupLog() *slog.Logger {
	if a.log != nil {
		return a.log
	}

	slvl := &slog.LevelVar{}
	slvl.Set(slog.LevelInfo)
	if a.Debug {
		slvl.Set(slog.LevelDebug)
	}

	log := slog.New(
		slog.NewTextHandler(
			os.Stderr,
			&slog.HandlerOptions{
				Level: slvl,
			}),
	)

	a.log = log
	return log
}

func (a *app) buildRemapper() (reimage.Remapper, *reimage.RecorderRemapper, error) {
	rm := reimage.MultiRemapper{}

	static, err := a.readStaticMappings()
	if err != nil {
		return nil, nil, fmt.Errorf("failed reading static remappings, %w", err)
	}

	if static != nil {
		rm = append(rm, static)
	}

	if static == nil {
		/*
			tagRemapper := &reimage.TagRemapper{
				CheckOnly: true,
				Logger:    a.log,
			}

			rm = append(rm, tagRemapper)
		*/

		if a.remoteTemplate != nil {
			rm = append(rm, &reimage.RenameRemapper{
				Ignore:     a.renameIgnore,
				RemotePath: a.RenameRemotePath,
				RemoteTmpl: a.remoteTemplate,
				Logger:     a.log,
			})
		}
	}

	recorder := &reimage.RecorderRemapper{}
	rm = append(rm, recorder)

	if !a.NoCopy {
		ensurer := &reimage.EnsureRemapper{
			NoClobber: !(a.Clobber),
			DryRun:    (a.DryRun),

			Logger: a.log,
		}
		rm = append(rm, ensurer)
	}

	return rm, recorder, nil
}

// checkVulns most of this should move into the main package
func (a *app) checkVulns(ctx context.Context, imgs map[string]reimage.QualifiedImage) error {
	c, err := containeranalysis.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("failed creating containeranalysis client, %w", err)
	}

	errs := make([]error, len(imgs))
	wg := &sync.WaitGroup{}
	wg.Add(len(imgs))
	i := 0
	for _, img := range imgs {
		go func(img reimage.QualifiedImage, i int) {
			defer wg.Done()

			vcCtx, vcCancel := context.WithTimeoutCause(ctx, a.VulnCheckTimeout, errors.New("timeout waiting for vuln-check"))
			defer vcCancel()

			checker := reimage.VulnChecker{
				IgnoreImages:  a.vulnCheckIgnoreImages,
				Parent:        a.VulnCheckGrafeasParent,
				Grafeas:       c.GetGrafeasClient(),
				MaxCVSS:       float32(a.VulnCheckMaxCVSS),
				CVEIgnoreList: a.VulnCheckIgnoreList,

				Logger: a.log,
			}
			ref, err := name.ParseReference(img.Tag)
			if err != nil {
				errs[i] = fmt.Errorf("could not parse ref %q, %w", img, err)
				return
			}

			desc, err := crane.Get(ref.String())
			if err != nil {
				errs[i] = fmt.Errorf("could not get ref %q, %w", ref.String(), err)
				return
			}

			digestStr := desc.Digest.String()
			dig := ref.Context().Registry.Repo(ref.Context().RepositoryStr()).Digest(digestStr)

			err = checker.Check(vcCtx, dig)
			if err != nil {
				errs[i] = fmt.Errorf("image check failed %q, %w", img, err)
				return
			}
		}(img, i)

		i++
	}

	wg.Wait()

	for _, err := range errs {
		switch {
		case errors.Is(err, context.Canceled):
			// if there are any context cancelled errors, we'll just return one
			// directly
			return err
		}
	}
	return errors.Join(errs...)
}

func main() {
	var err error
	app, err := setup()
	if err != nil {
		app.log.Error(fmt.Errorf("invalid options, %w", err).Error())
		os.Exit(1)
	}

	rm, recorder, err := app.buildRemapper()
	if err != nil {
		app.log.Error(err.Error())
		os.Exit(1)
	}

	s := &reimage.RemapUpdater{
		Remapper:                 rm,
		UnstructuredImagesFinder: app.imagFinder,
	}

	err = reimage.Process(os.Stdout, os.Stdin, s)
	if err != nil {
		app.log.Error(fmt.Errorf("failed processing input, %w", err).Error())
		os.Exit(1)
	}

	mappings, err := recorder.Mappings()
	if err != nil {
		app.log.Error(fmt.Errorf("mappings were invalid, %w", err).Error())
		os.Exit(1)
	}

	ctx := context.Background()

	err = app.checkVulns(ctx, mappings)
	if err != nil {
		app.log.Error(fmt.Errorf("vulncheck failed, %w", err).Error())
		os.Exit(1)
	}

	err = app.writeMappings(mappings)
	if err != nil {
		app.log.Error(fmt.Errorf("failed writing mappings, %w", err).Error())
		os.Exit(1)
	}
}
