package template

import (
	"bytes"
	"fmt"
	"strings"
	"text/template/parse"
)

// Default functions from text/template.
var builtins = map[string]interface{}{
	"printf": fmt.Sprintf,
}

// ToJs compiles a text/template to JavaScript. Bwahahaha.
func ToJs(name, template, namespace string) (js string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	treeSet, err := parse.Parse(name, template, "{{", "}}", builtins)
	if err != nil {
		return "", err
	}
	return new(jsCompiler).compile(treeSet, namespace), nil
}

// ----------------------------------------------------------------------------

// jsCompiler compiles a text/template to JavaScript. Bwahahaha.
//
// Adapted from com.google.template.soy.jssrc.internal.JsCodeBuilder,
// from the Closure Templates library. Copyright 2008 Google Inc.
type jsCompiler struct {
	b                *bytes.Buffer
	indent           string
	outputVars       []string
	outputVarsInited []bool
	delayed          []string
}

// outputVar returns the current output variable name.
func (c *jsCompiler) outputVar() string {
	if len(c.outputVars) > 0 {
		return c.outputVars[len(c.outputVars)-1]
	}
	panic("output variable name is not set")
}

// outputVarInited returns whether the current output variable is initialized.
func (c *jsCompiler) outputVarInited() bool {
	if len(c.outputVarsInited) > 0 {
		return c.outputVarsInited[len(c.outputVarsInited)-1]
	}
	panic("output variable initialization flag is not set")
}

// pushOutputVar sets a new current output variable name.
func (c *jsCompiler) pushOutputVar(name string) {
	c.outputVars = append(c.outputVars, name)
	c.outputVarsInited = append(c.outputVarsInited, false)
}

// popOutputVar removes the current output variable name. The previous output
// variable becomes the current.
func (c *jsCompiler) popOutputVar() {
	if len(c.outputVars) > 0 {
		c.outputVars = c.outputVars[:len(c.outputVars)-1]
		c.outputVarsInited = c.outputVarsInited[:len(c.outputVarsInited)-1]
	}
}

// initOutputVar appends a full line/statement for initializing the current
// output variable.
func (c *jsCompiler) initOutputVar() {
	if c.outputVarInited() {
		// Nothing to do since it's already initialized.
		return
	}
	c.writeLine("var ", c.outputVar(), " = new soy.StringBuilder();")
	c.setOutputVarInited()
}

// setOutputVarInited sets that the current output variable has already been
// initialized. This causes initOutputVar and addToOutputVar to not add
// initialization code even on the first use of the variable.
func (c *jsCompiler) setOutputVarInited() {
	if len(c.outputVarsInited) > 0 {
		c.outputVarsInited[len(c.outputVarsInited)-1] = true
	} else {
		panic("output variable is not set")
	}
}

// increaseIndent increases the current indent by two spaces.
func (c *jsCompiler) increaseIndent() {
	c.indent += "  "
}

// decreaseIndent decreases the current indent by two spaces.
func (c *jsCompiler) decreaseIndent() {
	// check out of range?
	c.indent = c.indent[:len(c.indent)-2]
}

// write appends one or more strings to the generated code.
func (c *jsCompiler) write(parts ...string) *jsCompiler {
	for _, v := range parts {
		c.b.WriteString(v)
	}
	return c
}

// writeIndent appends the current indent to the generated code.
func (c *jsCompiler) writeIndent() *jsCompiler {
	c.b.WriteString(c.indent)
	return c
}

// writeLine is equivalent to c.writeIndent().write(part, "\n").
func (c *jsCompiler) writeLine(parts ...string) *jsCompiler {
	c.writeIndent()
	c.write(parts...)
	c.b.WriteByte('\n')
	return c
}

// writeOutputVarName appends the name of the current output variable.
func (c *jsCompiler) writeOutputVarName() *jsCompiler {
	c.b.WriteString(c.outputVar())
	return c
}

// addToOutputVar appends a line/statement with the given concatenation of the
// given JS expressions saved to the current output variable.
func (c *jsCompiler) addToOutputVar(exprs ...string) {
	args := strings.Join(exprs, ", ")
	if c.outputVarInited() {
		// output.append(AAA, BBB);
		c.writeLine(c.outputVar(), ".append(", args, ");")
	} else {
		// var output = new soy.StringBuilder(AAA, BBB);
		c.writeLine("var ", c.outputVar(), " = new soy.StringBuilder(",
			args, ");")
		c.setOutputVarInited()
	}
}

// addDelayedToOutputVar appends delayed expressions to the current output
// variable.
func (c *jsCompiler) addDelayedToOutputVar() {
	if len(c.delayed) > 0 {
		c.addToOutputVar(c.delayed...)
		c.delayed = c.delayed[:0]
	}
}

// WIP
func (c *jsCompiler) compile(treeSet map[string]*parse.Tree, namespace string) string {
	c.b = new(bytes.Buffer)
	// Set a header.
	c.writeLine("// Code generated by gorilla/template.")
	c.writeLine("// Please don't edit this file by hand.")
	c.writeLine()
	// Declare namespaces.
	ns := ""
	namespace = strings.Trim(namespace, ".")
	for _, name := range strings.Split(namespace, ".") {
		if name != "" {
			if ns != "" {
				ns += "."
			}
			ns += name
			c.writeLine(fmt.Sprintf(
				"if (typeof %s == 'undefined') { var %s = {}; }", ns, ns))
		}
	}
	if namespace != "" {
		c.writeLine()
		namespace += "."
	}
	// Set a function for each template tree.
	for name, tree := range treeSet {
		c.pushOutputVar("output")
		c.writeLine(fmt.Sprintf("%s%s = function(opt_data, opt_sb) {",
			namespace, name))
		c.increaseIndent()
		c.writeLine("var output = opt_sb || new soy.StringBuilder();")
		c.setOutputVarInited()
		for _, node := range tree.Root.Nodes {
			c.visit(node)
		}
		c.addDelayedToOutputVar()
		c.writeLine("return opt_sb ? '' : output.toString();")
		c.decreaseIndent()
		c.writeLine("};")
		c.popOutputVar()
	}
	return c.b.String()
}

func (c *jsCompiler) visit(node parse.Node) {
	if node == nil {
		return
	}
	switch n := node.(type) {
	case *parse.ActionNode:
		c.visitActionNode(n)
	case *parse.BoolNode:
		c.visitBoolNode(n)
	case *parse.CommandNode:
		c.visitCommandNode(n)
	case *parse.DotNode:
		c.visitDotNode(n)
	case *parse.FieldNode:
		c.visitFieldNode(n)
	case *parse.IdentifierNode:
		c.visitIdentifierNode(n)
	case *parse.IfNode:
		c.visitIfNode(n)
	case *parse.ListNode:
		c.visitListNode(n)
	case *parse.NumberNode:
		c.visitNumberNode(n)
	case *parse.PipeNode:
		c.visitPipeNode(n)
	case *parse.RangeNode:
		c.visitRangeNode(n)
	case *parse.StringNode:
		c.visitStringNode(n)
	case *parse.TemplateNode:
		c.visitTemplateNode(n)
	case *parse.TextNode:
		c.visitTextNode(n)
	case *parse.VariableNode:
		c.visitVariableNode(n)
	case *parse.WithNode:
		c.visitWithNode(n)
	default:
		panic(fmt.Errorf("unexpected node type %T", n))
	}
}

func (c *jsCompiler) visitActionNode(n *parse.ActionNode) {
	// ...
}

func (c *jsCompiler) visitBoolNode(n *parse.BoolNode) {
	// ...
}

func (c *jsCompiler) visitCommandNode(n *parse.CommandNode) {
	// ...
}

func (c *jsCompiler) visitDotNode(n *parse.DotNode) {
	// ...
}

func (c *jsCompiler) visitFieldNode(n *parse.FieldNode) {
	// ...
}

func (c *jsCompiler) visitIdentifierNode(n *parse.IdentifierNode) {
	// ...
}

func (c *jsCompiler) visitIfNode(n *parse.IfNode) {
	// ...
}

func (c *jsCompiler) visitListNode(n *parse.ListNode) {
	// ...
}

func (c *jsCompiler) visitNumberNode(n *parse.NumberNode) {
	// ...
}

func (c *jsCompiler) visitPipeNode(n *parse.PipeNode) {
	// ...
}

func (c *jsCompiler) visitRangeNode(n *parse.RangeNode) {
	// ...
}

func (c *jsCompiler) visitStringNode(n *parse.StringNode) {
	// ...
}

func (c *jsCompiler) visitTemplateNode(n *parse.TemplateNode) {
	// ...
}

func (c *jsCompiler) visitTextNode(n *parse.TextNode) {
	c.delayed = append(c.delayed, "'" + string(n.Text) + "'")
}

func (c *jsCompiler) visitVariableNode(n *parse.VariableNode) {
	// ...
}

func (c *jsCompiler) visitWithNode(n *parse.WithNode) {
	// ...
}
