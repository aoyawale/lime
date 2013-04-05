package main

import (
	"fmt"
	"io/ioutil"
	"lime/backend"
	"lime/backend/primitives"
	"os/exec"
	"reflect"
	"regexp"
	"strings"
)

var re = regexp.MustCompile(`\p{Lu}`)

func pyname(in string) string {
	if in == "String" {
		return "Str"
	}
	return re.ReplaceAllStringFunc(in, func(a string) string { return "_" + strings.ToLower(a) })
}

func pytype(t reflect.Type) (string, error) {
	switch t.Kind() {
	case reflect.Slice:
		return "", fmt.Errorf("Can't handle type %s", t.Kind())
	case reflect.Ptr:
		t = t.Elem()
		if t.Kind() != reflect.Struct {
			return "", fmt.Errorf("Only supports struct pointers: ", t.Kind())
		}
		fallthrough
	case reflect.Struct:
		return "*" + t.Name(), nil
	case reflect.Int:
		return "*py.Int", nil
	case reflect.String:
		return "*py.String", nil
	case reflect.Bool:
		return "*py.Bool", nil
	default:
		return "", fmt.Errorf("Can't handle type %s", t.Kind())
	}
}

func pyretvar(name string, ot reflect.Type) (string, error) {
	switch ot.Kind() {
	case reflect.Ptr:
		ot = ot.Elem()
		if ot.Kind() != reflect.Struct {
			return "", fmt.Errorf("Only supports struct pointers: ", ot.Kind())
		}
		fallthrough
	case reflect.Struct:
		return fmt.Sprintf(`
			py%s, err = %sClass.Alloc(1)
			if err != nil {
			} else if v2, ok := py%s.(*%s); !ok {
				return nil, fmt.Errorf("Unable to convert return value to the right type?!: %%s", py%s.Type())
			} else {
				v2.data = %s
			}`, name, pyname(ot.Name()), name, ot.Name(), name, name), nil
	case reflect.Bool:
		return fmt.Sprintf(`
			if %s {
				py%s = py.True
			} else {
				py%s = py.False
			}`, name, name, name), nil
	case reflect.Int:
		return fmt.Sprintf("\n\tpy%s = py.NewInt(%s)", name, name), nil
	case reflect.String:
		return fmt.Sprintf("\n\tpy%s, err = py.NewString(%s)", name, name), nil
	default:
		return "", fmt.Errorf("Can't handle return type %s", ot.Kind())
	}
}

func pyret(ot reflect.Type) (string, error) {
	if v, err := pyretvar("ret", ot); err != nil {
		return "", err
	} else {
		return fmt.Sprintf(`
				var pyret py.Object
				var err error
				%s
				return pyret, err
				`, v), nil
	}
}

func pyacc(ot reflect.Type) string {
	switch ot.Kind() {
	case reflect.Ptr, reflect.Struct:
		return ".data"
	case reflect.Int:
		return ".Int()"
	case reflect.String:
		return ".String()"
	case reflect.Bool:
		return ".Bool()"
	default:
		panic(ot.Kind())
	}
}

func pytogoconv(in, set, name string, returnsValue bool, t reflect.Type) (string, error) {
	if t.Kind() == reflect.Map && t.Key().Kind() == reflect.String && t.Elem().Kind() == reflect.Interface {
		return fmt.Sprintf(`
		if v2, ok := %s.(*py.Dict); !ok {
			return nil, fmt.Errorf("Expected type *py.Dict for %s, not %%s", %s.Type())
		} else {
			if m, err := v2.MapString(); err != nil {
				return nil, err
			} else {
				for k, v := range m {
					switch t := v.(type) {
						case *py.Int:
							%s[k] = t.Int()
						case *py.Bool:
							%s[k] = t.Bool()
						case *py.String:
							%s[k] = t.String()
						case *py.Float:
							%s[k] = t.Float64()
						default:
							return nil, fmt.Errorf("Can't set key \"%%s\" with a type of %%s", k, v.Type())
					}
				}
			}
		}
`, in, name, in, set, set, set, set), nil
	}
	ty, err := pytype(t)
	if err != nil {
		return "", err
	}
	r := ""
	if returnsValue {
		r = "nil, "
	}
	return fmt.Sprintf(`
		if v2, ok := %s.(%s); !ok {
			return %sfmt.Errorf("Expected type %s for %s, not %%s", %s.Type())
		} else {
			%s = v2%s
		}`, in, ty, r, ty, name, in, set, pyacc(t)), nil
}

func generatemethods(t reflect.Type, ignorelist []string) (methods string) {
	t2 := t
	if t.Kind() == reflect.Ptr {
		t2 = t.Elem()
	}

	for i := 0; i < t.NumMethod(); i++ {
		var (
			ret  string
			m    = t.Method(i)
			args string
			rv   string
			in   = m.Type.NumIn() - 1
			out  = m.Type.NumOut()
			call string
		)
		if m.Name[0] != strings.ToUpper(m.Name[:1])[0] {
			goto skip
		}
		for _, j := range ignorelist {
			if m.Name == j {
				goto skip
			}
		}

		if in > 0 {
			args = "tu *py.Tuple, kw *py.Dict"
		}
		if m.Name == "String" {
			rv = "string"
		} else {
			rv = "(py.Object, error)"
		}

		ret += fmt.Sprintf("\nfunc (o *%s) Py%s(%s) %s {", t2.Name(), pyname(m.Name), args, rv)

		if in > 0 {
			ret += "\n\tvar ("
			for j := 1; j <= in; j++ {
				ret += fmt.Sprintf("\n\t\targ%d %s", j, m.Type.In(j))
			}
			r := ""
			if m.Name != "String" {
				r = "nil, "
			}
			ret += "\n\t)"

			for j := 1; j <= in; j++ {
				t := m.Type.In(j)
				name := fmt.Sprintf("arg%d", j)
				msg := fmt.Sprintf("%s.%s() %s", t2, m.Name, name)
				pygo, err := pytogoconv("v", name, msg, m.Name != "String", t)
				if err != nil {
					fmt.Printf("Skipping method %s.%s: %s\n", t2, m.Name, err)
					goto skip
				}
				if t.Kind() == reflect.Map && t.Key().Kind() == reflect.String {
					ret += fmt.Sprintf(`
						%s = make(%s)
						if v, err := tu.GetItem(%d); err == nil {%s}`, name, t, j-1, pygo)
				} else {
					ret += fmt.Sprintf(`
						if v, err := tu.GetItem(%d); err != nil {
							return %serr
						} else {%s}`, j-1, r, pygo)
				}
			}
		}

		if in > 0 {
			call = "o.data." + m.Name + "("
			for j := 1; j <= in; j++ {
				if j > 1 {
					call += ", "
				}
				call += fmt.Sprintf("arg%d", j)
			}
			call += ")"
		} else {
			call = "o.data." + m.Name + "()"
		}
		if m.Name == "String" {
			ret += "\n\treturn " + call
		} else if out > 0 {
			ret += "\n\t"
			for j := 0; j < out; j++ {
				if j > 0 {
					ret += ", "
				}
				ret += fmt.Sprintf("ret%d", j)
			}
			ret += " := " + call
			ret += "\nvar err error"
			for j := 0; j < out; j++ {
				ret += fmt.Sprintf("\nvar pyret%d py.Object\n", j)
				if r, err := pyretvar(fmt.Sprintf("ret%d", j), m.Type.Out(j)); err != nil {
					fmt.Printf("Skipping method %s.%s: %s\n", t2, m.Name, err)
					goto skip
				} else {
					ret += r
					ret += `
						if err != nil {
							// TODO: do the py objs need to be freed?
							return nil, err
						}
						`
				}
			}
			if out == 1 {
				ret += "\n\treturn pyret0, err"
			} else {
				ret += "\n\treturn py.PackTuple("
				for j := 0; j < out; j++ {
					if j > 0 {
						ret += ", "
					}
					ret += fmt.Sprintf("pyret%d", j)
				}
				ret += ")"
			}
		} else {
			ret += "\n\t" + call + "\n\treturn py.None, nil"
		}
		ret += "\n}\n"
		methods += ret
		//fmt.Printf("Created method %s.%s\n", t2, m.Name)
		continue
	skip:
		fmt.Printf("Skipping method %s.%s\n", t2, m.Name)
	}
	return
}

func generateWrapper(ptr reflect.Type, canCreate bool, ignorelist []string) (ret string) {
	t := ptr
	if t.Kind() == reflect.Ptr {
		t = ptr.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(t.Kind())
	}
	it := t.String()
	if !canCreate {
		it = "*" + it
	}
	ret += fmt.Sprintf(`
		var %sClass = py.Class{
			Name:    "sublime.%s",
			Pointer: (*%s)(nil),
		}

		type %s struct {
			py.BaseObject
			data %s
		}
		`, pyname(t.Name()), t.Name(), t.Name(), t.Name(), it)

	cons := ""
	if canCreate {
		cons = fmt.Sprintf(`
			func (o *%s) PyInit(args *py.Tuple, kwds *py.Dict) error {
				if args.Size() > %d {
					return fmt.Errorf("Expected at most %d arguments")
				}
			`, t.Name(), t.NumField(), t.NumField())
		ok := true
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			pygo, err := pytogoconv("v", "o.data."+f.Name, t.Name()+"."+f.Name, false, f.Type)
			if err != nil {
				ok = false
				break
			}
			cons += fmt.Sprintf(`
					if args.Size() > %d {
						if v, err := args.GetItem(%d); err != nil {
							return err
						} else {%s
						}
					}
				`, i, i, pygo)
		}
		if !ok {
			cons = ""
		} else {
			cons += "\n\treturn nil\n}"
		}
	}
	if cons == "" {
		ret += fmt.Sprintf(`
			func (o *%s) PyInit(args *py.Tuple, kwds *py.Dict) error {
				return fmt.Errorf("Can't initialize type %s")
			}`, t.Name(), t.Name())
	}
	ret += cons
	ret += generatemethods(ptr, ignorelist)
	if ptr.Kind() != reflect.Struct {
		ret += generatemethods(t, ignorelist)
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.Anonymous && f.Name[0] == strings.ToUpper(f.Name[:1])[0] {
			for _, j := range ignorelist {
				if f.Name == j {
					goto skip
				}
			}

			if r, err := pyret(f.Type); err != nil {
				fmt.Printf("Skipping field %s.%s: %s\n", t.Name(), f.Name, err)
			} else if pygo, err := pytogoconv("v", "o.data."+f.Name, t.Name()+"."+f.Name, false, f.Type); err != nil {
				fmt.Printf("Skipping field %s.%s: %s\n", t.Name(), f.Name, err)
			} else {
				ret += fmt.Sprintf(`
					func (o *%s) PyGet%s() (py.Object, error) {
						ret := o.data.%s%s
					}

					func (o *%s) PySet%s(v py.Object) error {%s
						return  nil
					}

					`, t.Name(), pyname(f.Name), f.Name, r,
					t.Name(), pyname(f.Name), pygo,
				)
			}
		skip:
		}
	}

	return
}

func main() {
	data := [][]string{
		{"../backend/sublime/region.go", generateWrapper(reflect.TypeOf(primitives.Region{}), true, nil)},
		{"../backend/sublime/regionset.go", generateWrapper(reflect.TypeOf(&primitives.RegionSet{}), false, []string{"Less", "Swap", "Adjust"})},
		{"../backend/sublime/edit.go", generateWrapper(reflect.TypeOf(&backend.Edit{}), false, []string{"Apply", "Undo"})},
		{"../backend/sublime/view.go", generateWrapper(reflect.TypeOf(&backend.View{}), false, []string{"Buffer", "Syntax"})},
		{"../backend/sublime/window.go", generateWrapper(reflect.TypeOf(&backend.Window{}), false, nil)},
		{"../backend/sublime/settings.go", generateWrapper(reflect.TypeOf(&backend.Settings{}), false, []string{"Parent", "Set", "Get"})},
	}
	for _, gen := range data {
		wr := `// This file was generated as part of a build step and shouldn't be manually modified
			package sublime

			import (
				"fmt"
				"lime/3rdparty/libs/gopy/lib"
				"lime/backend"
				"lime/backend/primitives"
			)
			var (
				_ = backend.View{}
				_ = primitives.Region{}
			)
			` + gen[1]
		if err := ioutil.WriteFile(gen[0], []byte(wr), 0644); err != nil {
			panic(err)
		} else {
			c := exec.Command("go", "fmt", gen[0])
			if o, err := c.CombinedOutput(); err != nil {
				panic(err)
			} else {
				fmt.Println(string(o))
			}
		}
	}
}