package parser

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
)

const maxContextIterations = 32

// Parser is a tool for parsing terraform templates at a given file system location
type Parser struct {
	hclParser *hclparse.Parser
	files     map[string]bool
}

// New creates a new Parser
func New() *Parser {
	return &Parser{
		hclParser: hclparse.NewParser(),
		files:     make(map[string]bool),
	}
}

// ParseDirectory recursively parses all terraform files within a given directory
func (parser *Parser) ParseDirectory(path string) (Blocks, error) {

	if err := parser.recursivelyParseDirectory(path, newPathBreaker(path)); err != nil {
		return nil, err
	}

	var blocks hcl.Blocks

	for _, file := range parser.hclParser.Files() {
		fileBlocks, err := parser.parseFile(file)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, fileBlocks...)
	}

	inputVars := make(map[string]cty.Value)
	// TODO add .tfvars values to inputVars

	allBlocks, _ := parser.buildEvaluationContext(
		blocks,
		path,
		inputVars,
		true,
		newPathBreaker(path),
	)
	return allBlocks.RemoveDuplicates(), nil
}

func (parser *Parser) parseFile(file *hcl.File) (hcl.Blocks, error) {

	contents, diagnostics := file.Body.Content(terraformSchema)
	if diagnostics != nil && diagnostics.HasErrors() {
		return nil, diagnostics
	}

	if contents == nil {
		return nil, fmt.Errorf("file contents is empty")
	}

	return contents.Blocks, nil
}

func (parser *Parser) recursivelyParseDirectory(path string, pb pathBreaker) error {

	files, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".") { //ignore dotfiles (including .terraform!)
			continue
		}
		fullPath := filepath.Join(path, file.Name())
		if exists := parser.files[fullPath]; exists {
			continue
		}

		parser.files[fullPath] = true
		if file.IsDir() {
			// We want local files to be loaded as needed by modules, but
			// we want to defend against directories being loaded multiple times.
			if pb.hasSeen(fullPath) {
				continue
			}
			pb.add(fullPath)

			if err := parser.recursivelyParseDirectory(fullPath, pb); err != nil {
				return err
			}
		} else if strings.HasSuffix(file.Name(), ".tf") {
			_, diagnostics := parser.hclParser.ParseHCLFile(fullPath)
			if diagnostics != nil && diagnostics.HasErrors() {
				return diagnostics
			}
		}
	}

	return nil
}

// BuildEvaluationContext creates an *hcl.EvalContext by parsing values for all terraform variables (where available) then interpolating values into resource, local and data blocks until all possible values can be constructed
func (parser *Parser) buildEvaluationContext(
	blocks hcl.Blocks,
	path string,
	inputVars map[string]cty.Value,
	isRoot bool,
	pb pathBreaker,
) (Blocks, *hcl.EvalContext) {
	ctx := &hcl.EvalContext{
		Variables: make(map[string]cty.Value),
	}

	ctx.Variables["module"] = cty.ObjectVal(make(map[string]cty.Value))

	moduleBlocks := make(map[string]Blocks)

	for i := 0; i < maxContextIterations; i++ {

		ctx.Variables["var"] = parser.getValuesByBlockType(ctx, blocks, "variable", inputVars)
		ctx.Variables["local"] = parser.getValuesByBlockType(ctx, blocks, "locals", nil)
		ctx.Variables["provider"] = parser.getValuesByBlockType(ctx, blocks, "provider", nil)
		resources := parser.getValuesByBlockType(ctx, blocks, "resource", nil)
		for key, resource := range resources.AsValueMap() {
			ctx.Variables[key] = resource
		}
		ctx.Variables["data"] = parser.getValuesByBlockType(ctx, blocks, "data", nil)

		if isRoot {
			ctx.Variables["output"] = parser.getValuesByBlockType(ctx, blocks, "output", nil)
		} else {
			outputs := parser.getValuesByBlockType(ctx, blocks, "output", nil)
			for key, val := range outputs.AsValueMap() {
				ctx.Variables[key] = val
			}
		}

		for _, moduleBlock := range blocks.OfType("module") {
			if len(moduleBlock.Labels) == 0 {
				continue
			}
			moduleMap := ctx.Variables["module"].AsValueMap()
			if moduleMap == nil {
				moduleMap = make(map[string]cty.Value)
			}
			moduleName := moduleBlock.Labels[0]

			moduleBlocks[moduleName], moduleMap[moduleName] = parser.parseModuleBlock(moduleBlock, ctx, path, pb) // todo return parsed blocks here too
			ctx.Variables["module"] = cty.ObjectVal(moduleMap)
		}

		// todo check of ctx has changed since last iteration - break if not
	}

	var localBlocks []*Block
	for _, block := range blocks {
		localBlocks = append(localBlocks, NewBlock(block, ctx))
	}

	for moduleName, blocks := range moduleBlocks {
		for _, block := range blocks {
			block.prefix = fmt.Sprintf("module.%s", moduleName)
			localBlocks = append(localBlocks, block)
		}
	}

	return localBlocks, ctx
}

func (parser *Parser) parseModuleBlock(
	block *hcl.Block,
	parentContext *hcl.EvalContext,
	rootPath string,
	pb pathBreaker,
) (Blocks, cty.Value) {

	if len(block.Labels) == 0 {
		return nil, cty.NilVal
	}

	inputVars := make(map[string]cty.Value)

	var source string
	attrs, _ := block.Body.JustAttributes()
	for _, attr := range attrs {

		inputVars[attr.Name], _ = attr.Expr.Value(parentContext)

		if attr.Name == "source" {
			sourceVal, _ := attr.Expr.Value(parentContext)
			if sourceVal.Type() == cty.String {
				source = sourceVal.AsString()
			}
		}
	}

	if source == "" {
		return nil, cty.NilVal
	}

	if !strings.HasPrefix(source, "./") && !strings.HasPrefix(source, "../") {
		// TODO support module registries/github etc.
		return nil, cty.NilVal
	}

	path := filepath.Join(rootPath, source)

	// We need to respect the module's path if it's local to the filesystem.
	// If the `rootPath` != `modulePath` then this means that we're not
	// parsing this module from the correct working directory, and so
	// parsing will break. In that case, reset the path to the path known
	// to the module so that local paths will work as expected.
	modulePath := filepath.Dir(block.DefRange.Filename)
	if rootPath != modulePath {
		path = modulePath
	}

	subParser := New()

	if err := subParser.recursivelyParseDirectory(path, pb); err != nil {
		return nil, cty.NilVal
	}

	var blocks []*hcl.Block

	for _, file := range subParser.hclParser.Files() {
		fileBlocks, err := subParser.parseFile(file)
		if err != nil {
			return nil, cty.NilVal
		}
		blocks = append(blocks, fileBlocks...)
	}

	childModules, ctx := subParser.buildEvaluationContext(blocks, path, inputVars, false, pb)

	return childModules, cty.ObjectVal(ctx.Variables)
}

// returns true if all evaluations were successful
func (parser *Parser) readValues(ctx *hcl.EvalContext, block *hcl.Block) cty.Value {

	values := make(map[string]cty.Value)

	attributes, diagnostics := block.Body.JustAttributes()
	if diagnostics != nil && diagnostics.HasErrors() {
		return cty.NilVal
	}

	for _, attribute := range attributes {
		val, _ := attribute.Expr.Value(ctx)
		values[attribute.Name] = val
	}

	return cty.ObjectVal(values)
}

// returns true if all evaluations were successful
func (parser *Parser) getValuesByBlockType(ctx *hcl.EvalContext, blocks hcl.Blocks, blockType string, inputVars map[string]cty.Value) cty.Value {

	blocksOfType := blocks.OfType(blockType)
	values := make(map[string]cty.Value)

	for _, block := range blocksOfType {

		switch block.Type {
		case "variable": // variables are special in that their value comes from the "default" attribute
			if len(block.Labels) < 1 {
				continue
			}
			attributes, _ := block.Body.JustAttributes()
			if attributes == nil {
				continue
			}
			if override, exists := inputVars[block.Labels[0]]; exists {
				values[block.Labels[0]] = override
			} else if def, exists := attributes["default"]; exists {
				values[block.Labels[0]], _ = def.Expr.Value(ctx)
			}
		case "output":
			if len(block.Labels) < 1 {
				continue
			}
			attributes, _ := block.Body.JustAttributes()
			if attributes == nil {
				continue
			}
			if def, exists := attributes["value"]; exists {
				values[block.Labels[0]], _ = def.Expr.Value(ctx)
			}
		case "locals":
			for key, val := range parser.readValues(ctx, block).AsValueMap() {
				values[key] = val
			}
		case "provider", "module":
			if len(block.Labels) < 1 {
				continue
			}
			values[block.Labels[0]] = parser.readValues(ctx, block)
		case "resource", "data":

			if len(block.Labels) < 2 {
				continue
			}

			blockMap, ok := values[block.Labels[0]]
			if !ok {
				values[block.Labels[0]] = cty.ObjectVal(make(map[string]cty.Value))
				blockMap = values[block.Labels[0]]
			}

			valueMap := blockMap.AsValueMap()
			if valueMap == nil {
				valueMap = make(map[string]cty.Value)
			}

			valueMap[block.Labels[1]] = parser.readValues(ctx, block)
			values[block.Labels[0]] = cty.ObjectVal(valueMap)
		}

	}

	return cty.ObjectVal(values)

}

// pathBreaker is a set of known paths to allow us to implement circuit breaking
// so that we can defend against infinity recursion in specific module
// sourcing circumstances.
type pathBreaker map[string]struct{}

// newPathBreaker creates a new path breaker based on the given original path.
func newPathBreaker(original string) pathBreaker {
	return map[string]struct{}{
		original: struct{}{},
	}
}

// add adds a new, now, known path to our path circuit breaker.
func (p pathBreaker) add(path string) {
	p[path] = struct{}{}
}

// hasSeen returns a boolean denoting if we've seen the given path before.
func (p pathBreaker) hasSeen(path string) bool {
	_, ok := p[path]
	return ok
}
