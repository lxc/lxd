package db

import (
	"fmt"
	"go/ast"
	"net/url"
	"reflect"
	"sort"
	"strings"

	"github.com/lxc/lxd/lxd/db/generate/lex"
	"github.com/lxc/lxd/shared"
	"github.com/pkg/errors"
)

// Packages returns the the AST packages in which to search for structs.
//
// By default it includes the lxd/db and shared/api packages.
func Packages() (map[string]*ast.Package, error) {
	packages := map[string]*ast.Package{}

	for _, name := range defaultPackages {
		pkg, err := lex.Parse(name)
		if err != nil {
			return nil, errors.Wrapf(err, "Parse %q", name)
		}
		parts := strings.Split(name, "/")
		packages[parts[len(parts)-1]] = pkg
	}

	return packages, nil
}

var defaultPackages = []string{
	"github.com/lxc/lxd/shared/api",
	"github.com/lxc/lxd/lxd/db",
}

// Filters parses all filtering statement defined for the given entity. It
// returns all supported combinations of filters, sorted by number of criteria.
func Filters(pkg *ast.Package, kind string, entity string) [][]string {
	objects := pkg.Scope.Objects
	filters := [][]string{}

	prefix := fmt.Sprintf("%s%sBy", lex.Minuscule(lex.Camel(entity)), lex.Camel(kind))

	for name := range objects {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		filters = append(filters, strings.Split(rest, "And"))
	}

	return sortFilters(filters)
}

// RefFilters parses all filtering statement defined for the given entity reference.
func RefFilters(pkg *ast.Package, entity string, ref string) [][]string {
	objects := pkg.Scope.Objects
	filters := [][]string{}

	prefix := fmt.Sprintf("%s%sRefBy", lex.Minuscule(lex.Camel(entity)), lex.Capital(ref))

	for name := range objects {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		filters = append(filters, strings.Split(rest, "And"))
	}

	return sortFilters(filters)
}

func sortFilters(filters [][]string) [][]string {
	sort.Slice(filters, func(i, j int) bool {
		n1 := len(filters[i])
		n2 := len(filters[j])
		if n1 != n2 {
			return n1 > n2
		}
		f1 := sortFilter(filters[i])
		f2 := sortFilter(filters[j])
		for k := range f1 {
			if f1[k] == f2[k] {
				continue
			}
			return f1[k] > f2[k]
		}
		panic("duplicate filter")
	})
	return filters
}

func sortFilter(filter []string) []string {
	f := make([]string, len(filter))
	copy(f, filter)
	sort.Sort(sort.Reverse(sort.StringSlice(f)))
	return f
}

// Criteria returns a list of criteria
func Criteria(pkg *ast.Package, entity string) ([]string, error) {
	name := fmt.Sprintf("%sFilter", lex.Camel(entity))
	str := findStruct(pkg.Scope, name)

	if str == nil {
		return nil, fmt.Errorf("No filter declared for %q", entity)
	}

	criteria := []string{}

	for _, f := range str.Fields.List {
		if len(f.Names) != 1 {
			return nil, fmt.Errorf("Unexpected fields number")
		}

		if !f.Names[0].IsExported() {
			return nil, fmt.Errorf("Unexported field name")
		}

		criteria = append(criteria, f.Names[0].Name)
	}

	return criteria, nil
}

// Parse the structure declaration with the given name found in the given Go
// package.
func Parse(pkg *ast.Package, name string, kind string) (*Mapping, error) {
	str := findStruct(pkg.Scope, name)
	if str == nil {
		return nil, fmt.Errorf("No declaration found for %q", name)
	}

	fields, err := parseStruct(str, kind)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to parse %q", name)
	}

	filterStr := findStruct(pkg.Scope, name+"Filter")
	if filterStr == nil {
		return nil, fmt.Errorf("No declaration found for %q", name+"Filter")
	}

	filters, err := parseStruct(filterStr, kind)
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to parse %q", name)
	}

	m := &Mapping{
		Package: pkg.Name,
		Name:    name,
		Fields:  fields,
		Filters: filters,
	}

	for _, filter := range filters {
		// Filter field must be present in original struct.
		field, err := m.FilterFieldByName(filter.Name)
		if err != nil {
			return nil, err
		}

		// Filter field's indirect reference must be present in the Filter struct.
		if field.IsIndirect() {
			indirectField := lex.Camel(field.Config.Get("via"))
			_, err := m.FilterFieldByName(indirectField)
			if err != nil {
				return nil, fmt.Errorf("field %q requires field %q in struct %q", field.Name, indirectField, name+"Filter")
			}
		}
	}

	return m, nil
}

// Find the StructType node for the structure with the given name
func findStruct(scope *ast.Scope, name string) *ast.StructType {
	obj := scope.Lookup(name)
	if obj == nil {
		return nil
	}

	typ, ok := obj.Decl.(*ast.TypeSpec)
	if !ok {
		return nil
	}

	str, ok := typ.Type.(*ast.StructType)
	if !ok {
		return nil
	}

	return str
}

// Extract field information from the given structure.
func parseStruct(str *ast.StructType, kind string) ([]*Field, error) {
	fields := make([]*Field, 0)

	for _, f := range str.Fields.List {
		if len(f.Names) == 0 {
			// Check if this is a parent struct.
			ident, ok := f.Type.(*ast.Ident)
			if !ok {
				continue
			}

			typ, ok := ident.Obj.Decl.(*ast.TypeSpec)
			if !ok {
				continue
			}

			parentStr, ok := typ.Type.(*ast.StructType)
			if !ok {
				continue
			}

			parentFields, err := parseStruct(parentStr, kind)
			if err != nil {
				return nil, errors.Wrapf(err, "Failed to parse parent struct")
			}
			fields = append(fields, parentFields...)

			continue
		}

		if len(f.Names) != 1 {
			return nil, fmt.Errorf("Expected a single field name, got %q", f.Names)
		}

		field, err := parseField(f, kind)
		if err != nil {
			return nil, err
		}

		// Don't add field if it has been ignored.
		if field != nil {
			fields = append(fields, field)
		}
	}

	return fields, nil
}

func parseField(f *ast.Field, kind string) (*Field, error) {
	name := f.Names[0]

	if !name.IsExported() {
		//return nil, fmt.Errorf("Unexported field name %q", name.Name)
	}

	// Ignore fields that are marked with a tag of `db:"ingore"`
	if f.Tag != nil {
		tag := f.Tag.Value
		tagValue := reflect.StructTag(tag[1 : len(tag)-1]).Get("db")
		if tagValue == "ignore" {
			return nil, nil
		}
	}

	typeObj := &Type{}

	typeName := parseType(typeObj, f.Type)
	if typeName == "" {
		return nil, fmt.Errorf("Unsupported type for field %q", name.Name)
	}

	typeObj.Name = typeName

	if IsColumnType(typeName) {
		typeObj.Code = TypeColumn
	} else if strings.HasPrefix(typeName, "[]") {
		typeObj.Code = TypeSlice
	} else if strings.HasPrefix(typeName, "map[") {
		typeObj.Code = TypeMap
	} else {
		return nil, fmt.Errorf("Unsupported type for field %q", name.Name)
	}

	var config url.Values
	if f.Tag != nil {
		tag := f.Tag.Value
		var err error
		config, err = url.ParseQuery(reflect.StructTag(tag[1 : len(tag)-1]).Get("db"))
		if err != nil {
			return nil, errors.Wrap(err, "Parse 'db' structure tag")
		}
	}

	// Ignore fields that are marked with `db:"omit"`.
	if omit := config.Get("omit"); omit != "" {
		omitFields := strings.Split(omit, ",")
		kind = strings.Replace(lex.Snake(kind), "_", "-", -1)
		if shared.StringInSlice(kind, omitFields) {
			return nil, nil
		} else if kind == "exists" && shared.StringInSlice("id", omitFields) {
			// Exists checks ID, so if we are omitting the field from ID, also omit it from Exists.
			return nil, nil
		}
	}

	field := Field{
		Name:   name.Name,
		Type:   *typeObj,
		Config: config,
	}

	return &field, nil
}

func parseType(typeObj *Type, x ast.Expr) string {
	switch t := x.(type) {
	case *ast.StarExpr:
		typeObj.IsPointer = true
		return parseType(typeObj, t.X)
	case *ast.SelectorExpr:
		return parseType(typeObj, t.X) + "." + t.Sel.String()
	case *ast.Ident:
		s := t.String()
		if s == "byte" {
			return "uint8"
		}
		return s
	case *ast.ArrayType:
		return "[" + parseType(typeObj, t.Len) + "]" + parseType(typeObj, t.Elt)
	case *ast.MapType:
		return "map[" + parseType(typeObj, t.Key) + "]" + parseType(typeObj, t.Value)
	case *ast.BasicLit:
		return t.Value
	case nil:
		return ""
	default:
		return ""
	}
}
