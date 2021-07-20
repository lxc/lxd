package db

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/lxc/lxd/lxd/db/generate/lex"
	"github.com/lxc/lxd/shared"
)

// Mapping holds information for mapping database tables to a Go structure.
type Mapping struct {
	Package string   // Package of the Go struct
	Name    string   // Name of the Go struct.
	Fields  []*Field // Metadata about the Go struct.
	Filters []*Field // Metadata about the Go struct used for filter fields.
}

// NaturalKey returns the struct fields that can be used as natural key for
// uniquely identifying a row in the underlying table (==.
//
// By convention the natural key field is the one called "Name", unless
// specified otherwise with the `db:natural_key` tags.
func (m *Mapping) NaturalKey() []*Field {
	key := []*Field{}

	for _, field := range m.Fields {
		if field.Config.Get("primary") != "" {
			key = append(key, field)
		}
	}

	if len(key) == 0 {
		// Default primary key.
		key = append(key, m.FieldByName("Name"))
	}

	return key
}

// ContainsFields checks that the mapping contains fields with the same type
// and name of given ones.
func (m *Mapping) ContainsFields(fields []*Field) bool {
	matches := map[*Field]bool{}

	for _, field := range m.Fields {
		for _, other := range fields {
			if field.Name == other.Name && field.Type.Name == other.Type.Name {
				matches[field] = true
			}
		}
	}

	return len(matches) == len(fields)
}

// FieldByName returns the field with the given name, if any.
func (m *Mapping) FieldByName(name string) *Field {
	for _, field := range m.Fields {
		if field.Name == name {
			return field
		}
	}

	return nil
}

// FieldColumnName returns the column name of the field with the given name,
// prefixed with the entity's table name.
func (m *Mapping) FieldColumnName(name string) string {
	field := m.FieldByName(name)
	return fmt.Sprintf("%s.%s", entityTable(m.Name), field.Column())
}

// FilterFieldByName returns the field with the given name if that field can be
// used as query filter, an error otherwise.
func (m *Mapping) FilterFieldByName(name string) (*Field, error) {
	for _, field := range m.Filters {
		if field.Name == name {
			if f := m.FieldByName(field.Name); f != nil {
				if field.Type.Code != TypeColumn {
					return nil, fmt.Errorf("Unknown filter %q not a column", name)
				}

				// Use the filter struct's type instead to deal with zero values.
				f.Type = field.Type

				return f, nil
			}
		}
	}
	return nil, fmt.Errorf("Unknown filter %q", name)
}

// FilterCombinations returns the power set of the filter fields, excluding combinations where indirect fields are present without their reference fields.
func (m *Mapping) FilterCombinations() [][]string {
	powerSet := [][]string{{}}
	for _, field := range m.Filters {
		var tmp [][]string
		for _, ps := range powerSet {
			lastSlice := append([]string{}, ps...)
			nextSlice := append(lastSlice, field.Name)
			tmp = append(tmp, nextSlice)
		}
		powerSet = append(powerSet, tmp...)
	}

	// Check for sets with indirect fields and no reference fields.
	validSet := [][]string{}
	for _, filterSet := range powerSet {
		for _, filter := range filterSet {
			// Get the main struct field for its tags.
			field := m.FieldByName(filter)
			if field == nil {
				return [][]string{}
			}

			if field.IsIndirect() {
				referenceField := lex.Camel(field.Config.Get("via"))
				if shared.StringInSlice(field.Name, filterSet) && !shared.StringInSlice(referenceField, filterSet) {
					// Make the invalid set nil to ignore later.
					filterSet = nil
					break
				}
			}
		}
		if filterSet != nil {
			validSet = append(validSet, filterSet)
		}
	}
	return validSet
}

// ColumnFields returns the fields that map directly to a database column,
// either on this table or on a joined one.
func (m *Mapping) ColumnFields(exclude ...string) []*Field {
	fields := []*Field{}

	for _, field := range m.Fields {
		if shared.StringInSlice(field.Name, exclude) {
			continue
		}
		if field.Type.Code == TypeColumn {
			fields = append(fields, field)
		}
	}

	return fields
}

// ScalarFields returns the fields that map directly to a single database
// column on another table that can be joined to this one.
func (m *Mapping) ScalarFields() []*Field {
	fields := []*Field{}

	for _, field := range m.Fields {
		if field.Config.Get("join") != "" {
			fields = append(fields, field)
		}
	}

	return fields
}

// RefFields returns the fields that are one-to-many references to other
// tables.
func (m *Mapping) RefFields() []*Field {
	fields := []*Field{}

	for _, field := range m.Fields {
		if field.Type.Code == TypeSlice || field.Type.Code == TypeMap {
			fields = append(fields, field)
		}
	}

	return fields
}

// Field holds all information about a field in a Go struct that is relevant
// for database code generation.
type Field struct {
	Name    string
	Type    Type
	Primary bool // Whether this field is part of the natural primary key.
	Config  url.Values
}

// Stmt must be used only on a non-columnar field. It returns the name of
// statement that should be used to fetch this field. A statement with that
// name must have been generated for the entity at hand.
func (f *Field) Stmt() string {
	switch f.Name {
	case "UsedBy":
		return "used_by"
	default:
		return ""
	}
}

// IsScalar returns true if the field is a scalar column value from a joined table.
func (f *Field) IsScalar() bool {
	return f.Config.Get("join") != ""
}

// IsIndirect returns true if the field is a scalar column value from a joined
// table that in turn requires another join.
func (f *Field) IsIndirect() bool {
	return f.IsScalar() && f.Config.Get("via") != ""
}

// IsPrimary returns true if the field part of the natural key.
func (f *Field) IsPrimary() bool {
	return f.Config.Get("primary") != "" || f.Name == "Name"
}

// Column returns the name of the database column the field maps to. The type
// code of the field must be TypeColumn.
func (f *Field) Column() string {
	if f.Type.Code != TypeColumn {
		panic("attempt to get column name of non-column field")
	}

	column := lex.Snake(f.Name)

	join := f.Config.Get("join")
	if join != "" {
		column = fmt.Sprintf("%s AS %s", join, column)

	}

	return column
}

// ZeroValue returns the literal representing the zero value for this field. The type
// code of the field must be TypeColumn.
func (f *Field) ZeroValue() string {
	if f.Type.Code != TypeColumn {
		panic("attempt to get zero value of non-column field")
	}

	if f.Type.IsPointer {
		return "nil"
	}

	switch f.Type.Name {
	case "string":
		return `""`
	case "int", "instancetype.Type", "int64", "OperationType", "CertificateType":
		// FIXME: we use -1 since at the moment integer criteria are
		// required to be positive.
		return "-1"
	case "bool":
		return "false"
	default:
		panic("unsupported zero value")
	}
}

// FieldColumns converts thegiven fields to list of column names separated
// by a comma.
func FieldColumns(fields []*Field) string {
	columns := make([]string, len(fields))

	for i, field := range fields {
		columns[i] = field.Column()
	}

	return strings.Join(columns, ", ")
}

// FieldArgs converts the given fields to function arguments, rendering their
// name and type.
func FieldArgs(fields []*Field) string {
	args := make([]string, len(fields))
	for i, field := range fields {
		args[i] = fmt.Sprintf("%s %s", lex.Minuscule(field.Name), field.Type.Name)
	}

	return strings.Join(args, ", ")
}

// FieldParams converts the given fields to function parameters, rendering their
// name.
func FieldParams(fields []*Field) string {
	args := make([]string, len(fields))
	for i, field := range fields {
		args[i] = lex.Minuscule(field.Name)
	}

	return strings.Join(args, ", ")
}

// FieldCriteria converts the given fields to AND-separated WHERE criteria.
func FieldCriteria(fields []*Field) string {
	criteria := make([]string, len(fields))

	for i, field := range fields {
		criteria[i] = fmt.Sprintf("%s = ?", field.Column())
	}

	return strings.Join(criteria, " AND ")
}

// Type holds all information about a field in a field type that is relevant
// for database code generation.
type Type struct {
	Name      string
	Code      int
	IsPointer bool
}

// Possible type code.
const (
	TypeColumn = iota
	TypeSlice
	TypeMap
)

// IsColumnType returns true if the given type name is one mapping directly to
// a database column.
func IsColumnType(name string) bool {
	return shared.StringInSlice(name, columnarTypeNames)
}

var columnarTypeNames = []string{
	"bool",
	"instancetype.Type",
	"int",
	"int64",
	"OperationType",
	"CertificateType",
	"string",
	"time.Time",
}
