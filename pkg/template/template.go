package template

import (
	"bytes"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"text/template"

	"github.com/Masterminds/sprig"
	log "github.com/Sirupsen/logrus"
)

// Function contains the description of an exported template function
type Function struct {

	// Name is the function name to bind in the template
	Name string

	// Description provides help for the function
	Description string

	// Func is the reference to the actual function
	Func interface{}
}

// FunctionExporter is implemented by any plugins wishing to show help on the function it exports.
type FunctionExporter interface {
	// Funcs returns a list of special template functions of the form func(template.Context, arg1, arg2) interface{}
	Funcs() []Function
}

// Context is a marker interface for a user-defined struct that is passed into the template engine (as context)
// and accessible in the exported template functions.  Template functions can have the signature
// func(template.Context, arg1, arg2 ...) (string, error) and when functions like this are registered, the template
// engine will dynamically create and export a function of the form func(arg1, arg2...) (string, error) where
// the context instance becomes an out-of-band struct that can be mutated by functions.  This in essence allows
// structured data as output of the template, in addition to a string from evaluating the template.
type Context interface {
	// Funcs returns a list of special template functions of the form func(template.Context, arg1, arg2) interface{}
	Funcs() []Function
}

// Options contains parameters for customizing the behavior of the engine
type Options struct {

	// SocketDir is the directory for locating the socket file for
	// a template URL of the form unix://socket_file/path/to/resource
	SocketDir string
}

type defaultValue struct {
	Name  string
	Value interface{}
	Doc   string
}

// Template is the templating engine
type Template struct {
	options Options

	url      string
	body     []byte
	parsed   *template.Template
	funcs    map[string]interface{}
	binds    map[string]interface{}
	defaults map[string]defaultValue
	lock     sync.Mutex
}

// NewTemplate fetches the content at the url and returns a template.  If the string begins
// with str:// as scheme, then the rest of the string is interpreted as the body of the template.
func NewTemplate(s string, opt Options) (*Template, error) {
	var buff []byte
	contextURL := s
	// Special case of specifying the entire template as a string; otherwise treat as url
	if strings.Index(s, "str://") == 0 {
		buff = []byte(strings.Replace(s, "str://", "", 1))
		contextURL = defaultContextURL()
	} else {
		b, err := Fetch(s, opt)
		if err != nil {
			return nil, err
		}
		buff = b
	}
	return NewTemplateFromBytes(buff, contextURL, opt)
}

// NewTemplateFromBytes builds the template from buffer with a contextURL which is used to deduce absolute
// path of any 'included' templates e.g. {{ include "./another.tpl" . }}
func NewTemplateFromBytes(buff []byte, contextURL string, opt Options) (*Template, error) {
	if contextURL == "" {
		log.Warningln("Context is not known.  Included templates may not work properly.")
	}

	return &Template{
		options:  opt,
		url:      contextURL,
		body:     buff,
		funcs:    map[string]interface{}{},
		binds:    map[string]interface{}{},
		defaults: map[string]defaultValue{},
	}, nil
}

// SetOptions sets the runtime flags for the engine
func (t *Template) SetOptions(opt Options) *Template {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.options = opt
	return t
}

// AddFunc adds a new function to support in template
func (t *Template) AddFunc(name string, f interface{}) *Template {
	t.lock.Lock()
	defer t.lock.Unlock()
	t.funcs[name] = f
	return t
}

// Validate parses the template and checks for validity.
func (t *Template) Validate() (*Template, error) {
	t.lock.Lock()
	t.parsed = nil
	t.lock.Unlock()
	return t, t.build(nil)
}

func (t *Template) build(context Context) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	if t.parsed != nil {
		return nil
	}

	fm := t.DefaultFuncs()

	for k, v := range sprig.TxtFuncMap() {
		fm[k] = v
	}

	for k, v := range t.funcs {
		if tf, err := makeTemplateFunc(context, v); err == nil {
			fm[k] = tf
		} else {
			return err
		}
	}

	if context != nil {
		for _, f := range context.Funcs() {
			if tf, err := makeTemplateFunc(context, f.Func); err == nil {
				fm[f.Name] = tf
			} else {
				return err
			}
		}
	}

	parsed, err := template.New(t.url).Funcs(fm).Parse(string(t.body))
	if err != nil {
		return err
	}

	t.parsed = parsed
	return nil
}

// Execute is a drop-in replace of the execute method of template
func (t *Template) Execute(output io.Writer, context interface{}) error {
	if err := t.build(toContext(context)); err != nil {
		return err
	}
	return t.parsed.Execute(output, context)
}

func toContext(in interface{}) Context {
	var context Context
	if in != nil {
		if s, is := in.(Context); is {
			context = s
		}
	}
	return context
}

// Render renders the template given the context
func (t *Template) Render(context interface{}) (string, error) {
	if err := t.build(toContext(context)); err != nil {
		return "", err
	}
	var buff bytes.Buffer
	err := t.parsed.Execute(&buff, context)
	return buff.String(), err
}

// converts a function of f(Context, ags...) to a regular template function
func makeTemplateFunc(ctx Context, f interface{}) (interface{}, error) {

	contextType := reflect.TypeOf((*Context)(nil)).Elem()

	ff := reflect.Indirect(reflect.ValueOf(f))
	// first we check to see if f has the special signature where the first
	// parameter is the context parameter...
	if ff.Kind() != reflect.Func {
		return nil, fmt.Errorf("not a function:%v", f)
	}

	if ff.Type().NumIn() > 0 && ff.Type().In(0).AssignableTo(contextType) {

		in := make([]reflect.Type, ff.Type().NumIn()-1) // exclude the context param
		out := make([]reflect.Type, ff.Type().NumOut())

		for i := 1; i < ff.Type().NumIn(); i++ {
			in[i-1] = ff.Type().In(i)
		}
		variadic := false
		if len(in) > 0 {
			variadic = in[len(in)-1].Kind() == reflect.Slice
		}
		for i := 0; i < ff.Type().NumOut(); i++ {
			out[i] = ff.Type().Out(i)
		}
		funcType := reflect.FuncOf(in, out, variadic)
		funcImpl := func(in []reflect.Value) []reflect.Value {
			if !variadic {
				return ff.Call(append([]reflect.Value{reflect.ValueOf(ctx)}, in...))
			}

			variadicParam := in[len(in)-1]
			last := make([]reflect.Value, variadicParam.Len())
			for i := 0; i < variadicParam.Len(); i++ {
				last[i] = variadicParam.Index(i)
			}
			return ff.Call(append(append([]reflect.Value{reflect.ValueOf(ctx)}, in[0:len(in)-1]...), last...))
		}

		newFunc := reflect.MakeFunc(funcType, funcImpl)
		return newFunc.Interface(), nil
	}
	return ff.Interface(), nil
}
