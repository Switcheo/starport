package cosmosgen

import (
	"context"
	"embed"
	"io/ioutil"
	"os"
	"path/filepath"
	"text/template"

	"github.com/iancoleman/strcase"
	"github.com/otiai10/copy"
	"github.com/pkg/errors"
	"github.com/tendermint/starport/starport/pkg/cmdrunner"
	"github.com/tendermint/starport/starport/pkg/cmdrunner/step"
	"github.com/tendermint/starport/starport/pkg/cosmosanalysis/module"
	"github.com/tendermint/starport/starport/pkg/gomodule"
	"github.com/tendermint/starport/starport/pkg/nodetime/protobufjs"
	"github.com/tendermint/starport/starport/pkg/nodetime/sta"
	"github.com/tendermint/starport/starport/pkg/protoc"
	"github.com/tendermint/starport/starport/pkg/protopath"
	gomodmodule "golang.org/x/mod/module"
	"golang.org/x/sync/errgroup"
)

var (
	protocOuts = []string{
		"--gocosmos_out=plugins=interfacetype+grpc,Mgoogle/protobuf/any.proto=github.com/cosmos/cosmos-sdk/codec/types:.",
		"--grpc-gateway_out=logtostderr=true:.",
	}

	openAPIOut = []string{
		"--openapiv2_out=logtostderr=true,allow_merge=true:.",
	}

	sdkImport          = "github.com/cosmos/cosmos-sdk"
	sdkProto           = "proto"
	sdkProtoThirdParty = "third_party/proto"

	fileTypes = "types"
)

//go:embed templates/*
var templates embed.FS

// tpl holds the js client template which is for wrapping the generated protobufjs types and rest client,
// utilizing cosmjs' type registry, tx signing & broadcasting through exported, high level txClient() and queryClient() funcs.
var tpl = template.Must(
	template.New("client.js.tpl").
		Funcs(template.FuncMap{
			"camelCase": strcase.ToLowerCamel,
		}).
		ParseFS(templates, "templates/client.js.tpl"),
)

type generateOptions struct {
	gomodPath string
	jsOut     func(module.Module) string
}

// TODO add WithInstall.

// Target adds a new code generation target to Generate.
type Target func(*generateOptions)

// WithJSGeneration adds JS code generation. out hook is called for each module to
// retrieve the path that should be used to place generated js code inside for a given module.
func WithJSGeneration(out func(module.Module) (path string)) Target {
	return func(o *generateOptions) {
		o.jsOut = out
	}
}

// WithGoGeneration adds Go code generation.
func WithGoGeneration(gomodPath string) Target {
	return func(o *generateOptions) {
		o.gomodPath = gomodPath
	}
}

// generator generates code for sdk and sdk apps.
type generator struct {
	ctx          context.Context
	projectPath  string
	protoPath    string
	includePaths []string
	o            *generateOptions
	deps         []gomodmodule.Version
}

// Generate generates code from proto app's proto files.
// make sure that all paths are absolute.
func Generate(
	ctx context.Context,
	projectPath,
	protoPath string,
	includePaths []string,
	target Target,
	otherTargets ...Target,
) error {
	g := &generator{
		ctx:          ctx,
		projectPath:  projectPath,
		protoPath:    protoPath,
		includePaths: includePaths,
		o:            &generateOptions{},
	}

	for _, target := range append(otherTargets, target) {
		target(g.o)
	}

	if err := g.setup(); err != nil {
		return err
	}

	if g.o.gomodPath != "" {
		if err := g.generateGo(); err != nil {
			return err
		}
	}

	// js generation requires Go types to be existent in the source code.
	// so it needs to run after Go code gen.
	if g.o.jsOut != nil {
		if err := g.generateJS(); err != nil {
			return err
		}
	}

	return nil
}

func (g *generator) setup() (err error) {
	// Cosmos SDK hosts proto files of own x/ modules and some third party ones needed by itself and
	// blockchain apps. Generate should be aware of these and make them available to the blockchain
	// app that wants to generate code for its own proto.
	//
	// blockchain apps may use different versions of the SDK. following code first makes sure that
	// app's dependencies are download by 'go mod' and cached under the local filesystem.
	// and then, it determines which version of the SDK is used by the app and what is the absolute path
	// of its source code.
	if err := cmdrunner.
		New(cmdrunner.DefaultWorkdir(g.projectPath)).
		Run(g.ctx, step.New(step.Exec("go", "mod", "download"))); err != nil {
		return err
	}

	// parse the go.mod of the app and extract dependencies.
	modfile, err := gomodule.ParseAt(g.projectPath)
	if err != nil {
		return err
	}

	g.deps, err = gomodule.ResolveDependencies(modfile)

	return
}

func (g *generator) generateGo() error {
	includePaths, err := g.resolveInclude(protopath.NewModule(sdkImport, sdkProto, sdkProtoThirdParty))
	if err != nil {
		return err
	}

	// created a temporary dir to locate generated code under which later only some of them will be moved to the
	// app's source code. this also prevents having leftover files in the app's source code or its parent dir -when
	// command executed directly there- in case of an interrupt.
	tmp, err := ioutil.TempDir("", "")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// discover every sdk module.
	modules, err := module.Discover(g.projectPath)
	if err != nil {
		return err
	}

	// code generate for each module.
	for _, m := range modules {
		if err := protoc.Generate(g.ctx, tmp, m.Pkg.Path, includePaths, protocOuts); err != nil {
			return err
		}
	}

	// move generated code for the app under the relative locations in its source code.
	generatedPath := filepath.Join(tmp, g.o.gomodPath)
	err = copy.Copy(generatedPath, g.projectPath)
	return errors.Wrap(err, "cannot copy path")
}

func (g *generator) generateJS() error {
	jsIncludePaths, err := g.resolveInclude(protopath.NewModule(sdkImport, sdkProto))
	if err != nil {
		return err
	}

	oaiIncludePaths, err := g.resolveInclude(protopath.NewModule(sdkImport, sdkProto, sdkProtoThirdParty))
	if err != nil {
		return err
	}

	// generate generates JS code for a module.
	generate := func(ctx context.Context, m module.Module) error {
		out := g.o.jsOut(m)

		// generate protobufjs types for each module.
		err = protobufjs.Generate(
			ctx,
			out,
			fileTypes,
			m.Pkg.Path,
			jsIncludePaths,
		)
		if err != nil {
			return err
		}

		oaitemp, err := ioutil.TempDir("", "")
		if err != nil {
			return err
		}
		defer os.RemoveAll(oaitemp)

		// generate OpenAPI spec.
		err = protoc.Generate(
			ctx,
			oaitemp,
			m.Pkg.Path,
			oaiIncludePaths,
			openAPIOut,
		)
		if err != nil {
			return err
		}

		// generate the REST client from the OpenAPI spec.
		var (
			srcspec = filepath.Join(oaitemp, "apidocs.swagger.json")
			outjs   = filepath.Join(out, "rest.js")
		)
		if err := sta.Generate(ctx, outjs, srcspec, "2"); err != nil { // 2 points to sdk module name.
			return err
		}

		// generate the client, the js wrapper.
		outclient := filepath.Join(out, "index.js")
		f, err := os.OpenFile(outclient, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		defer f.Close()

		err = tpl.Execute(f, struct {
			Module    module.Module
			TypesPath string
			RESTPath  string
		}{
			m,
			"./types",
			"./rest",
		})

		return err
	}

	// sourcePaths keeps a list of root paths of Go projects (source codes) that might contain
	// Cosmos SDK modules inside.
	sourcePaths := []string{
		g.projectPath, // user's blockchain. may contain internal modules. it is the first place to look for.
	}

	// go through the Go dependencies (inside go.mod) of each source path, some of them might be hosting
	// Cosmos SDK modules that could be in use by user's blockchain.
	//
	// Cosmos SDK is a dependency of all blockchains, so it's absolute that we'll be discovering all modules of the
	// SDK as well during this process.
	//
	// even if a dependency contains some SDK modules, not all of these modules could be used by user's blockchain.
	// this is fine, we can still generate JS clients for those non modules, it is up to user to use (import in JS)
	// not use generated modules.
	// not used ones will never get resolved inside JS envrionment and will not ship to production, JS bundlers will avoid.
	//
	// TODO(ilgooz): we can still implement some sort of smart filtering to detect non used modules by the user's blockchain
	// at some point, it is a nice to have.
	for _, dep := range g.deps {
		deppath, err := gomodule.LocatePath(dep)
		if err != nil {
			return err
		}
		sourcePaths = append(sourcePaths, deppath)
	}

	gs := &errgroup.Group{}

	// try to discover SDK modules in all source paths.
	for _, sourcePath := range sourcePaths {
		sourcePath := sourcePath

		gs.Go(func() error {
			modules, err := module.Discover(sourcePath)
			if err != nil {
				return err
			}

			gg, ctx := errgroup.WithContext(g.ctx)

			// do code generation for each found module.
			for _, m := range modules {
				m := m

				gg.Go(func() error { return generate(ctx, m) })
			}

			return gg.Wait()
		})
	}

	return gs.Wait()
}

func (g *generator) resolveInclude(modules ...protopath.Module) (paths []string, err error) {
	includePaths, err := protopath.ResolveDependencyPaths(g.deps, modules...)
	if err != nil {
		return nil, err
	}
	includePaths = append([]string{g.protoPath}, includePaths...)
	includePaths = append(includePaths, g.includePaths...)
	return includePaths, nil
}
