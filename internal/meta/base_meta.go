package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Azure/aztfy/internal/client"
	"github.com/Azure/aztfy/internal/config"
	"github.com/Azure/aztfy/internal/log"
	"github.com/Azure/aztfy/internal/resmap"
	"github.com/Azure/aztfy/internal/utils"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-config-inspect/tfconfig"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/magodo/tfadd/providers/azurerm"
	"github.com/magodo/tfadd/tfadd"
	"github.com/magodo/tfmerge/tfmerge"
	"github.com/magodo/workerpool"
)

const ResourceMappingFileName = "aztfyResourceMapping.json"
const SkippedResourcesFileName = "aztfySkippedResources.txt"

type BaseMeta interface {
	Init() error
	DeInit() error
	Workspace() string
	ParallelImport(items []*ImportItem)
	PushState() error
	CleanTFState(addr string)
	GenerateCfg(ImportList) error
	ExportSkippedResources(l ImportList) error
	ExportResourceMapping(ImportList) error
	CleanUpWorkspace() error
}

var _ BaseMeta = &baseMeta{}

type baseMeta struct {
	subscriptionId string
	rootdir        string
	outdir         string
	tf             *tfexec.Terraform
	resourceClient *armresources.Client
	devProvider    bool
	backendType    string
	backendConfig  []string
	fullConfig     bool
	parallelism    int
	hclOnly        bool

	// The module address prefix in the resource addr. E.g. module.mod1.module.mod2.azurerm_resource_group.test.
	// This is an empty string if module path is not specified.
	moduleAddr string
	// The module directory in the fs where the terraform config should be generated to. This does not necessarily have the same structure as moduleAddr.
	// This is the same as the outdir if module path is not specified.
	moduleDir string

	// Parallel import supports
	importBaseDirs   []string
	importModuleDirs []string
	// The original base state, which is retrieved prior to the import, and is compared with the actual base state prior to the mutated state is pushed,
	// to ensure the base state has no out of band changes during the importing.
	originBaseState []byte
	// The current base state, which is mutated during the importing
	baseState []byte
	importTFs []*tfexec.Terraform

	// Use a safer name which is less likely to conflicts with users' existing files.
	// This is mainly used for the --append option.
	useSafeFilename bool
}

func NewBaseMeta(cfg config.CommonConfig) (*baseMeta, error) {
	if cfg.Parallelism == 0 {
		return nil, fmt.Errorf("Parallelism not set in the config")
	}

	// Initialize the rootdir
	cachedir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("error finding the user cache directory: %w", err)
	}
	rootdir := filepath.Join(cachedir, "aztfy")
	// #nosec G301
	if err := os.MkdirAll(rootdir, 0750); err != nil {
		return nil, fmt.Errorf("creating rootdir %q: %w", rootdir, err)
	}

	var (
		modulePaths []string
		moduleAddr  string
		moduleDir   = cfg.OutputDir
	)
	if cfg.ModulePath != "" {
		modulePaths = strings.Split(cfg.ModulePath, ".")

		var segs []string
		for _, moduleName := range modulePaths {
			segs = append(segs, "module."+moduleName)
		}
		moduleAddr = strings.Join(segs, ".")

		// Ensure the module path is something called by the main module
		// We are following the module source and recursively call the LoadModule below. This is valid since we only support local path modules.
		// (remote sources are not supported since we will end up generating config to that module, it only makes sense for local path modules)
		module, err := tfconfig.LoadModule(moduleDir)
		if err != nil {
			return nil, fmt.Errorf("loading main module: %v", err)
		}

		for i, moduleName := range modulePaths {
			mc := module.ModuleCalls[moduleName]
			if mc == nil {
				return nil, fmt.Errorf("no module %q invoked by the root module", strings.Join(modulePaths[:i+1], "."))
			}
			// See https://developer.hashicorp.com/terraform/language/modules/sources#local-paths
			if !strings.HasPrefix(mc.Source, "./") && !strings.HasPrefix(mc.Source, "../") {
				return nil, fmt.Errorf("the source of module %q is not a local path", strings.Join(modulePaths[:i+1], "."))
			}
			moduleDir = filepath.Join(moduleDir, mc.Source)
			module, err = tfconfig.LoadModule(moduleDir)
			if err != nil {
				return nil, fmt.Errorf("loading module %q: %v", strings.Join(modulePaths[:i+1], "."), err)
			}
		}
	}

	// Create the import directories
	var importBaseDirs []string
	var importModuleDirs []string
	for i := 0; i < cfg.Parallelism; i++ {
		dir, err := os.MkdirTemp("", "aztfy-")
		if err != nil {
			return nil, fmt.Errorf("creating import directory: %v", err)
		}

		// Creating the module hierarchy if module path is specified.
		// The hierarchy used here is not necessarily to be the same as is defined. What we need to guarantee here is the module address in TF is as specified.
		mdir := dir
		for _, moduleName := range modulePaths {
			fpath := filepath.Join(mdir, "main.tf")
			if err := utils.WriteFileSync(fpath, []byte(fmt.Sprintf(`module "%s" {
  source = "./%s"
}
`, moduleName, moduleName)), 0644); err != nil {
				return nil, fmt.Errorf("creating %s: %v", fpath, err)
			}

			mdir = path.Join(mdir, moduleName)
			// #nosec G301
			if err := os.Mkdir(mdir, 0750); err != nil {
				return nil, fmt.Errorf("creating module dir %s: %v", mdir, err)
			}
		}

		importModuleDirs = append(importModuleDirs, mdir)
		importBaseDirs = append(importBaseDirs, dir)
	}

	// Construct client builder
	b, err := client.NewClientBuilder()
	if err != nil {
		return nil, fmt.Errorf("building authorizer: %w", err)
	}
	resClient, err := b.NewResourcesClient(cfg.SubscriptionId)
	if err != nil {
		return nil, fmt.Errorf("new resource client")
	}

	// AzureRM provider will honor env.var "AZURE_HTTP_USER_AGENT" when constructing for HTTP "User-Agent" header.
	// #nosec G104
	os.Setenv("AZURE_HTTP_USER_AGENT", "aztfy")

	// Avoid the AzureRM provider to call the expensive RP listing API, repeatedly.
	// #nosec G104
	os.Setenv("ARM_PROVIDER_ENHANCED_VALIDATION", "false")
	// #nosec G104
	os.Setenv("ARM_SKIP_PROVIDER_REGISTRATION", "true")

	meta := &baseMeta{
		subscriptionId:   cfg.SubscriptionId,
		rootdir:          rootdir,
		outdir:           cfg.OutputDir,
		resourceClient:   resClient,
		devProvider:      cfg.DevProvider,
		backendType:      cfg.BackendType,
		backendConfig:    cfg.BackendConfig,
		fullConfig:       cfg.FullConfig,
		parallelism:      cfg.Parallelism,
		useSafeFilename:  cfg.Append,
		hclOnly:          cfg.HCLOnly,
		importBaseDirs:   importBaseDirs,
		importModuleDirs: importModuleDirs,

		moduleAddr: moduleAddr,
		moduleDir:  moduleDir,
	}

	return meta, nil
}

func (meta baseMeta) Workspace() string {
	return meta.outdir
}

func (meta *baseMeta) Init() error {
	ctx := context.TODO()

	if err := meta.initTF(ctx); err != nil {
		return err
	}

	if err := meta.initProvider(ctx); err != nil {
		return err
	}

	baseState, err := meta.tf.StatePull(ctx)
	if err != nil {
		return fmt.Errorf("failed to pull state: %v", err)
	}
	meta.baseState = []byte(baseState)
	meta.originBaseState = []byte(baseState)

	return nil
}

func (meta baseMeta) DeInit() error {
	// Clean up the temporary workspaces for parallel import
	for _, dir := range meta.importBaseDirs {
		// #nosec G104
		os.RemoveAll(dir)
	}
	return nil
}

func (meta *baseMeta) CleanTFState(addr string) {
	ctx := context.TODO()
	// #nosec G104
	meta.tf.StateRm(ctx, addr)
}

// Import multiple items in parallel. Note that the length of items have to be less or equal than the parallelism.
func (meta *baseMeta) ParallelImport(items []*ImportItem) {
	ctx := context.TODO()

	wp := workerpool.NewWorkPool(meta.parallelism)

	wp.Run(func(i interface{}) error {
		idx := i.(int)

		// Don't merge state if import error hit
		if items[idx].ImportError != nil {
			return nil
		}

		stateFile := filepath.Join(meta.importBaseDirs[idx], "terraform.tfstate")

		// Ensure the state file is removed after this round import, preparing for the next round.
		defer os.Remove(stateFile)

		log.Printf("[DEBUG] Merging terraform state file %s", stateFile)
		newState, err := tfmerge.Merge(ctx, meta.tf, meta.baseState, stateFile)
		if err != nil {
			items[idx].ImportError = fmt.Errorf("failed to merge state file: %v", err)
			return nil
		}
		meta.baseState = newState
		return nil
	})

	for i := range items {
		i := i
		wp.AddTask(func() (interface{}, error) {
			item := items[i]
			moduleDir := meta.importModuleDirs[i]
			tf := meta.importTFs[i]

			// Construct the empty cfg file for importing
			cfgFile := filepath.Join(moduleDir, meta.filenameTmpCfg())
			tpl := fmt.Sprintf(`resource "%s" "%s" {}`, item.TFAddr.Type, item.TFAddr.Name)
			if err := utils.WriteFileSync(cfgFile, []byte(tpl), 0644); err != nil {
				item.ImportError = fmt.Errorf("generating resource template file: %w", err)
				return i, nil
			}
			defer os.Remove(cfgFile)

			// Import resources
			addr := item.TFAddr.String()
			if meta.moduleAddr != "" {
				addr = meta.moduleAddr + "." + addr
			}
			log.Printf("[INFO] Importing %s as %s", item.TFResourceId, addr)
			err := tf.Import(ctx, addr, item.TFResourceId)
			item.ImportError = err
			item.Imported = err == nil
			return i, nil
		})
	}

	// #nosec G104
	wp.Done()
}

func (meta baseMeta) PushState() error {
	ctx := context.TODO()

	// Ensure there is no out of band change on the base state
	baseState, err := meta.tf.StatePull(ctx)
	if err != nil {
		return fmt.Errorf("failed to pull state: %v", err)
	}
	if baseState != string(meta.originBaseState) {
		edits := myers.ComputeEdits(span.URIFromPath("origin.tfstate"), string(meta.originBaseState), baseState)
		changes := fmt.Sprint(gotextdiff.ToUnified("origin.tfstate", "current.tfstate", string(meta.originBaseState), edits))
		return fmt.Errorf("there is out-of-band changes on the state file during running aztfy:\n%s", changes)
	}

	// Create a temporary state file to hold the merged states, then push the state to the output directory.
	f, err := os.CreateTemp("", "")
	if err != nil {
		return fmt.Errorf("creating a temporary state file: %v", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the temporary state file %s: %v", f.Name(), err)
	}
	if err := utils.WriteFileSync(f.Name(), meta.baseState, 0644); err != nil {
		return fmt.Errorf("writing to the temporary state file: %v", err)
	}

	defer os.Remove(f.Name())

	if err := meta.tf.StatePush(ctx, f.Name(), tfexec.Lock(true)); err != nil {
		return fmt.Errorf("failed to push state: %v", err)
	}

	return nil
}

func (meta baseMeta) GenerateCfg(l ImportList) error {
	return meta.generateCfg(l, meta.lifecycleAddon, meta.addDependency)
}

func (meta baseMeta) ExportResourceMapping(l ImportList) error {
	m := resmap.ResourceMapping{}
	for _, item := range l {
		if item.Skip() {
			continue
		}
		m[item.AzureResourceID.String()] = resmap.ResourceMapEntity{
			ResourceId:   item.TFResourceId,
			ResourceType: item.TFAddr.Type,
			ResourceName: item.TFAddr.Name,
		}
	}
	output := filepath.Join(meta.outdir, ResourceMappingFileName)
	b, err := json.MarshalIndent(m, "", "\t")
	if err != nil {
		return fmt.Errorf("JSON marshalling the resource mapping: %v", err)
	}
	if err := os.WriteFile(output, b, 0600); err != nil {
		return fmt.Errorf("writing the resource mapping to %s: %v", output, err)
	}
	return nil
}

func (meta baseMeta) ExportSkippedResources(l ImportList) error {
	var sl []string
	for _, item := range l {
		if item.Skip() {
			sl = append(sl, "- "+item.AzureResourceID.String())
		}
	}
	if len(sl) == 0 {
		return nil
	}

	output := filepath.Join(meta.outdir, SkippedResourcesFileName)
	if err := os.WriteFile(output, []byte(fmt.Sprintf(`Following resources are marked to be skipped:

%s
`, strings.Join(sl, "\n"))), 0600); err != nil {
		return fmt.Errorf("writing the skipped resources to %s: %v", output, err)
	}
	return nil
}

func (meta baseMeta) CleanUpWorkspace() error {
	// Clean up everything under the output directory, except for the TF code.
	if meta.hclOnly {
		tmpDir, err := os.MkdirTemp("", "")
		if err != nil {
			return err
		}
		defer func() {
			// #nosec G104
			os.RemoveAll(tmpDir)
		}()

		tmpMainCfg := filepath.Join(tmpDir, meta.filenameMainCfg())
		tmpProviderCfg := filepath.Join(tmpDir, meta.filenameProviderSetting())

		if err := utils.CopyFile(filepath.Join(meta.outdir, meta.filenameMainCfg()), tmpMainCfg); err != nil {
			return err
		}
		if err := utils.CopyFile(filepath.Join(meta.outdir, meta.filenameProviderSetting()), tmpProviderCfg); err != nil {
			return err
		}

		if err := utils.RemoveEverythingUnder(meta.outdir); err != nil {
			return err
		}

		if err := utils.CopyFile(tmpMainCfg, filepath.Join(meta.outdir, meta.filenameMainCfg())); err != nil {
			return err
		}
		if err := utils.CopyFile(tmpProviderCfg, filepath.Join(meta.outdir, meta.filenameProviderSetting())); err != nil {
			return err
		}
	}

	return nil
}

func (meta baseMeta) generateCfg(l ImportList, cfgTrans ...TFConfigTransformer) error {
	ctx := context.TODO()

	cfginfos, err := meta.stateToConfig(ctx, l)
	if err != nil {
		return fmt.Errorf("converting from state to configurations: %w", err)
	}
	cfginfos, err = meta.terraformMetaHook(cfginfos, cfgTrans...)
	if err != nil {
		return fmt.Errorf("Terraform HCL meta hook: %w", err)
	}

	return meta.generateConfig(cfginfos)
}

func (meta *baseMeta) terraformConfig(backendType string) string {
	if meta.devProvider {
		return fmt.Sprintf(`terraform {
  backend %q {}
}
`, backendType)
	}

	return fmt.Sprintf(`terraform {
  backend %q {}
  required_providers {
    azurerm = {
      source = "hashicorp/azurerm"
      version = "%s"
    }
  }
}
`, backendType, azurerm.ProviderSchemaInfo.Version)
}

func (meta *baseMeta) providerConfig() string {
	return fmt.Sprintf(`provider "azurerm" {
  features {}
}
`)
}

func (meta baseMeta) filenameTerraformSetting() string {
	if meta.useSafeFilename {
		return "terraform.aztfy.tf"
	}
	return "terraform.tf"
}

func (meta baseMeta) filenameProviderSetting() string {
	if meta.useSafeFilename {
		return "provider.aztfy.tf"
	}
	return "provider.tf"
}

func (meta baseMeta) filenameMainCfg() string {
	if meta.useSafeFilename {
		return "main.aztfy.tf"
	}
	return "main.tf"
}

func (meta baseMeta) filenameTmpCfg() string {
	return "tmp.aztfy.tf"
}

func (meta *baseMeta) initTF(ctx context.Context) error {
	log.Printf("[INFO] Init Terraform")
	tfDir := filepath.Join(meta.rootdir, "terraform")
	// #nosec G301
	if err := os.MkdirAll(tfDir, 0750); err != nil {
		return fmt.Errorf("creating terraform cache dir %q: %w", meta.rootdir, err)
	}
	execPath, err := FindTerraform(ctx, tfDir)
	if err != nil {
		return fmt.Errorf("error finding a terraform exectuable: %w", err)
	}
	log.Printf("[INFO] Find terraform binary at %s", execPath)

	newTF := func(dir string) (*tfexec.Terraform, error) {
		tf, err := tfexec.NewTerraform(dir, execPath)
		if err != nil {
			return nil, fmt.Errorf("error running NewTerraform: %w", err)
		}
		if v, ok := os.LookupEnv("TF_LOG_PATH"); ok {
			// #nosec G104
			tf.SetLogPath(v)
		}
		if v, ok := os.LookupEnv("TF_LOG"); ok {
			// #nosec G104
			tf.SetLog(v)
		}
		return tf, nil
	}

	tf, err := newTF(meta.outdir)
	if err != nil {
		return fmt.Errorf("failed to init terraform: %w", err)
	}
	meta.tf = tf

	for _, importDir := range meta.importBaseDirs {
		tf, err := newTF(importDir)
		if err != nil {
			return fmt.Errorf("failed to init terraform: %w", err)
		}
		meta.importTFs = append(meta.importTFs, tf)
	}

	return nil
}

func (meta *baseMeta) initProvider(ctx context.Context) error {
	log.Printf("[INFO] Init provider")

	module, err := tfconfig.LoadModule(meta.outdir)
	if err != nil {
		return err
	}

	if module.ProviderConfigs["azurerm"] == nil {
		log.Printf("[INFO] Output directory doesn't contain provider setting, create one then")
		cfgFile := filepath.Join(meta.outdir, meta.filenameProviderSetting())
		if err := utils.WriteFileSync(cfgFile, []byte(meta.providerConfig()), 0644); err != nil {
			return fmt.Errorf("error creating provider config: %w", err)
		}
	}

	if len(module.ProviderConfigs) == 0 {
		log.Printf("[INFO] Output directory doesn't contain terraform required provider setting, create one then")
		cfgFile := filepath.Join(meta.outdir, meta.filenameTerraformSetting())
		if err := utils.WriteFileSync(cfgFile, []byte(meta.terraformConfig(meta.backendType)), 0644); err != nil {
			return fmt.Errorf("error creating terraform config: %w", err)
		}
	}

	// Initialize provider for the output directory.
	var opts []tfexec.InitOption
	for _, opt := range meta.backendConfig {
		opts = append(opts, tfexec.BackendConfig(opt))
	}
	if err := meta.tf.Init(ctx, opts...); err != nil {
		return fmt.Errorf("error running terraform init: %s", err)
	}

	// Initialize provider for the import directories.
	for i := range meta.importBaseDirs {
		providerFile := filepath.Join(meta.importBaseDirs[i], "provider.tf")
		if err := utils.WriteFileSync(providerFile, []byte(meta.providerConfig()), 0644); err != nil {
			return fmt.Errorf("error creating provider config: %w", err)
		}
		terraformFile := filepath.Join(meta.importBaseDirs[i], "terraform.tf")
		if err := utils.WriteFileSync(terraformFile, []byte(meta.terraformConfig("local")), 0644); err != nil {
			return fmt.Errorf("error creating terraform config: %w", err)
		}
		if err := meta.importTFs[i].Init(ctx); err != nil {
			return fmt.Errorf("error running terraform init: %s", err)
		}
	}

	return nil
}

func (meta baseMeta) stateToConfig(ctx context.Context, list ImportList) (ConfigInfos, error) {
	out := ConfigInfos{}

	wp := workerpool.NewWorkPool(meta.parallelism)
	wp.Run(func(i interface{}) error {
		out = append(out, i.(ConfigInfo))
		return nil
	})

	for _, item := range list.Imported() {
		item := item
		wp.AddTask(func() (interface{}, error) {
			addr := item.TFAddr.String()
			if meta.moduleAddr != "" {
				addr = meta.moduleAddr + "." + addr
			}
			b, err := tfadd.State(ctx, meta.tf, tfadd.Target(addr), tfadd.Full(meta.fullConfig))
			if err != nil {
				return nil, fmt.Errorf("converting terraform state to config for resource %s: %w", item.TFAddr, err)
			}
			tpl := meta.cleanupTerraformAdd(string(b))
			f, diag := hclwrite.ParseConfig([]byte(tpl), "", hcl.InitialPos)
			if diag.HasErrors() {
				return nil, fmt.Errorf("parsing the HCL generated by \"terraform add\" of %s: %s", item.TFAddr, diag.Error())
			}
			return ConfigInfo{
				ImportItem: item,
				hcl:        f,
			}, nil
		})
	}

	if err := wp.Done(); err != nil {
		return nil, err
	}

	return out, nil
}

func (meta baseMeta) terraformMetaHook(configs ConfigInfos, cfgTrans ...TFConfigTransformer) (ConfigInfos, error) {
	var err error
	for _, trans := range cfgTrans {
		configs, err = trans(configs)
		if err != nil {
			return nil, err
		}
	}
	return configs, nil
}

func (meta baseMeta) generateConfig(cfgs ConfigInfos) error {
	cfgFile := filepath.Join(meta.moduleDir, meta.filenameMainCfg())
	buf := bytes.NewBuffer([]byte{})
	for _, cfg := range cfgs {
		if _, err := cfg.DumpHCL(buf); err != nil {
			return err
		}
		buf.Write([]byte("\n"))
	}
	if err := appendToFile(cfgFile, buf.String()); err != nil {
		return fmt.Errorf("generating main configuration file: %w", err)
	}

	return nil
}

func (meta baseMeta) cleanupTerraformAdd(tpl string) string {
	segs := strings.Split(tpl, "\n")
	// Removing:
	// - preceding/trailing state lock related log when TF backend is used.
	for len(segs) != 0 && strings.HasPrefix(segs[0], "Acquiring state lock.") {
		segs = segs[1:]
	}

	last := len(segs) - 1
	for last != -1 && (segs[last] == "" || strings.HasPrefix(segs[last], "Releasing state lock.")) {
		segs = segs[:last]
		last = len(segs) - 1
	}
	return strings.Join(segs, "\n")
}

// lifecycleAddon adds lifecycle meta arguments for some identified resources, which are mandatory to make them usable.
func (meta baseMeta) lifecycleAddon(configs ConfigInfos) (ConfigInfos, error) {
	out := make(ConfigInfos, len(configs))
	for i, cfg := range configs {
		switch cfg.TFAddr.Type {
		case "azurerm_application_insights_web_test":
			if err := hclBlockAppendLifecycle(cfg.hcl.Body().Blocks()[0].Body(), []string{"tags"}); err != nil {
				return nil, fmt.Errorf("azurerm_application_insights_web_test: %v", err)
			}
		}
		out[i] = cfg
	}
	return out, nil
}

func (meta baseMeta) addDependency(configs ConfigInfos) (ConfigInfos, error) {
	if err := configs.AddDependency(); err != nil {
		return nil, err
	}

	var out ConfigInfos

	configSet := map[string]ConfigInfo{}
	for _, cfg := range configs {
		configSet[cfg.AzureResourceID.String()] = cfg
	}

	for _, cfg := range configs {
		if len(cfg.DependsOn) != 0 {
			if err := hclBlockAppendDependency(cfg.hcl.Body().Blocks()[0].Body(), cfg.DependsOn, configSet); err != nil {
				return nil, err
			}
		}
		out = append(out, cfg)
	}

	return out, nil
}

func appendToFile(path, content string) error {
	// #nosec G304
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	// #nosec G307
	defer f.Close()

	_, err = f.WriteString(content)
	return err
}

func resourceNamePattern(p string) (prefix, suffix string) {
	if pos := strings.LastIndex(p, "*"); pos != -1 {
		return p[:pos], p[pos+1:]
	}
	return p, ""
}

func ptr[T any](v T) *T {
	return &v
}
