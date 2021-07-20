package db

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/lxc/lxd/lxd/db/generate/file"
	"github.com/lxc/lxd/lxd/db/generate/lex"
	"github.com/pkg/errors"
)

// Stmt generates a particular database query statement.
type Stmt struct {
	db       string                  // Target database (cluster or node)
	pkg      string                  // Package where the entity struct is declared.
	entity   string                  // Name of the database entity
	kind     string                  // Kind of statement to generate
	config   map[string]string       // Configuration parameters
	packages map[string]*ast.Package // Packages to perform for struct declaration lookups
}

// NewStmt return a new statement code snippet for running the given kind of
// query against the given database entity.
func NewStmt(database, pkg, entity, kind string, config map[string]string) (*Stmt, error) {
	packages, err := Packages()
	if err != nil {
		return nil, err
	}

	stmt := &Stmt{
		db:       database,
		pkg:      pkg,
		entity:   entity,
		kind:     kind,
		config:   config,
		packages: packages,
	}

	return stmt, nil
}

// Generate plumbing and wiring code for the desired statement.
func (s *Stmt) Generate(buf *file.Buffer) error {
	if strings.HasPrefix(s.kind, "create") && strings.HasSuffix(s.kind, "-ref") {
		return s.createRef(buf)
	}

	if strings.HasPrefix(s.kind, "delete") && strings.HasSuffix(s.kind, "-ref") {
		return s.deleteRef(buf)
	}

	if strings.HasSuffix(s.kind, "-ref") {
		return s.ref(buf)
	}

	switch s.kind {
	case "objects":
		return s.objects(buf)
	case "create":
		return s.create(buf, false)
	case "create-or-replace":
		return s.create(buf, true)
	case "id":
		return s.id(buf)
	case "rename":
		return s.rename(buf)
	case "update":
		return s.update(buf)
	case "delete":
		return s.delete(buf)
	case "names":
		return s.names(buf)
	default:
		return fmt.Errorf("Unknown statement '%s'", s.kind)
	}
}

func (s *Stmt) objects(buf *file.Buffer) error {
	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return err
	}

	where := ""
	filterCombinations := mapping.FilterCombinations()
	for _, filters := range filterCombinations {
		if len(filters) > 0 {
			where = "WHERE "
			for i, filter := range filters {
				if i > 0 {
					where += "AND "
				}

				if filter == "Parent" {
					where += fmt.Sprintf("SUBSTR(%s.name,1,?)=? ", lex.Plural(s.entity))
					continue
				}

				field, err := mapping.FilterFieldByName(filter)
				if err != nil {
					return err
				}

				var column string
				if field.IsScalar() {
					column = lex.Snake(field.Name)
				} else {
					column = mapping.FieldColumnName(field.Name)
				}

				comparison, ok := field.Config["comparison"]
				if !ok {
					comparison = []string{"equal"}
				}
				switch comparison[0] {
				case "equal":
					where += fmt.Sprintf("%s = ? ", column)
				case "like":
					where += fmt.Sprintf("%s LIKE ? ", column)
				default:
					panic("unknown 'comparison' value")
				}
			}
		}

		boiler := stmts["objects"]
		fields := mapping.ColumnFields()
		columns := make([]string, len(fields))
		for i, field := range fields {
			if field.IsScalar() {
				columns[i] = field.Column()
			} else {
				columns[i] = mapping.FieldColumnName(field.Name)
				coalesce, ok := field.Config["coalesce"]
				if ok {
					columns[i] = fmt.Sprintf("coalesce(%s, %s)", columns[i], coalesce[0])
				}
			}
		}
		nk := mapping.NaturalKey()
		orderBy := make([]string, len(nk))
		for i, field := range nk {
			if field.IsScalar() {
				orderBy[i] = lex.Plural(lex.Snake(field.Name)) + ".id"
			} else {
				orderBy[i] = mapping.FieldColumnName(field.Name)
			}
		}

		table := entityTable(s.entity)
		for _, field := range mapping.ScalarFields() {
			join := field.Config.Get("join")
			right := strings.Split(join, ".")[0]
			via := entityTable(s.entity)
			if field.Config.Get("via") != "" {
				via = entityTable(field.Config.Get("via"))
			}
			table += fmt.Sprintf(" JOIN %s ON %s.%s_id = %s.id", right, via, lex.Singular(right), right)
		}

		sql := fmt.Sprintf(boiler, strings.Join(columns, ", "), table, where, strings.Join(orderBy, ", "))

		s.register(buf, sql, filters...)
	}
	return nil
}

func (s *Stmt) names(buf *file.Buffer) error {
	mapping, err := Parse(s.packages[s.pkg], lex.Capital(s.entity), s.kind)
	if err != nil {
		return err
	}

	table := entityTable(s.entity)
	for _, field := range mapping.ScalarFields() {
		join := field.Config.Get("join")
		right := strings.Split(join, ".")[0]
		via := entityTable(s.entity)
		if field.Config.Get("via") != "" {
			via = entityTable(field.Config.Get("via"))
		}
		table += fmt.Sprintf(" JOIN %s ON %s.%s_id = %s.id", right, via, lex.Singular(right), right)
	}

	nk := mapping.NaturalKey()
	columns := make([]string, len(nk))
	orderBy := make([]string, len(nk))
	for i, field := range nk {
		if field.IsScalar() {
			columns[i] = field.Column()
			orderBy[i] = lex.Plural(lex.Snake(field.Name)) + ".id"
		} else {
			columns[i] = mapping.FieldColumnName(field.Name)
			orderBy[i] = mapping.FieldColumnName(field.Name)
		}
	}

	where := ""

	for _, filters := range mapping.FilterCombinations() {
		for i, filter := range filters {
			if i == 0 {
				where = "WHERE "
			}

			field, err := mapping.FilterFieldByName(filter)

			if err != nil {
				return err
			}

			if i > 0 {
				where += "AND "
			}

			var column string
			if field.IsScalar() {
				column = lex.Snake(field.Name)
			} else {
				column = mapping.FieldColumnName(field.Name)
			}

			where += fmt.Sprintf("%s = ? ", column)
		}

		boiler := stmts["names"]
		sql := fmt.Sprintf(boiler, strings.Join(columns, ", "), table, where, strings.Join(orderBy, ", "))
		s.register(buf, sql, filters...)
	}
	return nil
}

func (s *Stmt) ref(buf *file.Buffer) error {
	// Base snake-case name of the references (e.g. "used-by-ref" -> "used_by")
	name := strings.Replace(s.kind[:strings.Index(s.kind, "-ref")], "-", "_", -1)

	// Object type of the reference
	typ := lex.Camel(name)

	// Table name where reference objects can be fetched.
	table := fmt.Sprintf("%s_%s_ref", entityTable(s.entity), name)

	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return err
	}

	field := mapping.FieldByName(typ)
	if field == nil {
		return fmt.Errorf("Entity %s has no field named %s", s.entity, typ)
	}

	nk := mapping.NaturalKey()

	var columns []string

	if IsColumnType(lex.Element(field.Type.Name)) {
		columns = make([]string, len(nk)+1)
		for i, field := range nk {
			columns[i] = lex.Snake(field.Name)
		}
		columns[len(columns)-1] = "value"
	} else if field.Type.Name == "map[string]string" {
		// By default we consider string->string maps of "config" type
		columns = make([]string, len(nk)+2)
		for i, field := range nk {
			columns[i] = lex.Snake(field.Name)
		}
		columns[len(columns)-2] = "key"
		columns[len(columns)-1] = "value"
	} else if field.Type.Name == "map[string]map[string]string" {
		// By default we consider string->map[string]string maps of "device" type
		columns = make([]string, len(nk)+4)
		for i, field := range nk {
			columns[i] = lex.Snake(field.Name)
		}
		columns[len(columns)-4] = "device"
		columns[len(columns)-3] = "type"
		columns[len(columns)-2] = "key"
		columns[len(columns)-1] = "value"
	} else {
		ref, err := Parse(s.packages["db"], typ, s.kind)
		if err != nil {
			return errors.Wrap(err, "Parse referenced entity")
		}

		// Check that the reference object contains the primary key of the
		// entity.
		if !ref.ContainsFields(nk) {
			return fmt.Errorf("Reference type %s does not contain %s's primary key", typ, s.entity)
		}

		//columns = FieldColumns(ref.Fields)
	}

	where := ""
	for _, filters := range mapping.FilterCombinations() {

		for i, filter := range filters {
			if i == 0 {
				where = "WHERE "
			}

			field, err := mapping.FilterFieldByName(filter)

			if err != nil {
				return err
			}

			if i > 0 {
				where += "AND "
			}

			column := lex.Snake(field.Name)
			where += fmt.Sprintf("%s = ? ", column)
		}

		orderBy := make([]string, len(nk))
		for i, field := range nk {
			orderBy[i] = lex.Snake(field.Name)
		}

		sql := fmt.Sprintf(
			"SELECT %s FROM %s %sORDER BY %s", strings.Join(columns, ", "),
			table, where, strings.Join(orderBy, ", "))

		s.register(buf, sql, filters...)
	}

	return nil
}

func (s *Stmt) create(buf *file.Buffer, replace bool) error {
	// Support using a different structure or package to pass arguments to Create.
	entityCreate, ok := s.config["struct"]
	if !ok {
		entityCreate = entityPost(s.entity)
	}

	mapping, err := Parse(s.packages[s.pkg], entityCreate, s.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}
	all := mapping.ColumnFields("ID") // This exclude the ID column, which is autogenerated.
	via := map[string][]*Field{}      // Map scalar fields to their additional indirect fields

	// Filter out indirect fields
	fields := []*Field{}
	for _, field := range all {
		if field.IsIndirect() {
			entity := field.Config.Get("via")
			via[entity] = append(via[entity], field)
			continue
		}
		fields = append(fields, field)
	}

	columns := make([]string, len(fields))
	params := make([]string, len(fields))

	for i, field := range fields {

		if field.IsScalar() {
			ref := lex.Snake(field.Name)
			columns[i] = ref + "_id"
			table := entityTable(ref)
			params[i] = fmt.Sprintf("(SELECT %s.id FROM %s", table, table)
			for _, other := range via[ref] {
				otherRef := lex.Snake(other.Name)
				otherTable := entityTable(otherRef)
				params[i] += fmt.Sprintf(" JOIN %s ON %s.id = %s.%s_id", otherTable, otherTable, table, otherRef)
			}
			params[i] += fmt.Sprintf(" WHERE")
			for _, other := range via[ref] {
				otherRef := lex.Snake(other.Name)
				otherTable := entityTable(otherRef)
				params[i] += fmt.Sprintf(" %s.name = ? AND", otherTable)
			}
			params[i] += fmt.Sprintf(" %s.name = ?)", table)
		} else {
			columns[i] = field.Column()
			params[i] = "?"
		}
	}

	tmpl := stmts[s.kind]
	if replace {
		tmpl = stmts["replace"]
	}

	sql := fmt.Sprintf(
		tmpl, entityTable(s.entity),
		strings.Join(columns, ", "), strings.Join(params, ", "))
	s.register(buf, sql)

	return nil
}

func (s *Stmt) createRef(buf *file.Buffer) error {
	// Base snake-case name of the references (e.g. "used-by-ref" -> "used_by")
	name := strings.Replace(s.kind[len("create-"):strings.Index(s.kind, "-ref")], "-", "_", -1)

	// Field name of the reference
	fieldName := lex.Camel(name)

	// Table name where reference objects can be fetched.
	table := fmt.Sprintf("%s_%s", entityTable(s.entity), name)

	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return err
	}

	field := mapping.FieldByName(fieldName)
	if field == nil {
		return fmt.Errorf("Entity %s has no field named %s", s.entity, fieldName)
	}

	if field.Type.Name == "map[string]string" {
		// Assume this is a config table
		columns := fmt.Sprintf("%s_id, key, value", s.entity)
		params := "?, ?, ?"

		sql := fmt.Sprintf(stmts["create"], table, columns, params)
		s.register(buf, sql)
	} else if field.Type.Name == "map[string]map[string]string" {
		// Assume this is a devices table
		columns := fmt.Sprintf("%s_id, name, type", s.entity)
		params := "?, ?, ?"

		sql := fmt.Sprintf(stmts["create"], table, columns, params)
		s.register(buf, sql)

		columns = fmt.Sprintf("%s_device_id, key, value", s.entity)
		params = "?, ?, ?"

		sql = fmt.Sprintf(stmts["create"], table+"_config", columns, params)

		kind := fmt.Sprintf("Create%sConfigRef", field.Name)
		buf.L("const %s = %s.RegisterStmt(`\n%s\n`)", stmtCodeVar(s.entity, kind), s.db, sql)
	}

	return nil
}

func (s *Stmt) id(buf *file.Buffer) error {
	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}
	sql := naturalKeySelect(s.entity, mapping)
	s.register(buf, sql)

	return nil
}

func (s *Stmt) rename(buf *file.Buffer) error {
	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return err
	}

	table := entityTable(s.entity)
	where := whereClause(mapping.NaturalKey())

	sql := fmt.Sprintf(stmts[s.kind], table, where)
	s.register(buf, sql)
	return nil
}

func (s *Stmt) update(buf *file.Buffer) error {
	// Support using a different structure or package to pass arguments to Create.
	entityUpdate, ok := s.config["struct"]
	if !ok {
		entityUpdate = entityPut(s.entity)
	}

	mapping, err := Parse(s.packages[s.pkg], entityUpdate, s.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}
	fields := mapping.ColumnFields("ID") // This exclude the ID column, which is autogenerated.

	updates := make([]string, len(fields))

	for i, field := range fields {
		if field.IsScalar() {
			// TODO: make this more general
			ref := lex.Snake(field.Name)
			updates[i] = fmt.Sprintf("%s_id = ", ref)
			updates[i] += fmt.Sprintf("(SELECT id FROM %s WHERE name = ?)", lex.Plural(ref))
		} else {
			updates[i] = fmt.Sprintf("%s = ?", field.Column())
		}
	}

	sql := fmt.Sprintf(
		stmts[s.kind], entityTable(s.entity),
		strings.Join(updates, ", "), "id = ?")
	s.register(buf, sql)

	return nil
}

func (s *Stmt) delete(buf *file.Buffer) error {
	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return err
	}

	table := entityTable(s.entity)

	for _, filters := range mapping.FilterCombinations() {
		fields := []*Field{}
		for _, filter := range filters {
			field, err := mapping.FilterFieldByName(filter)
			if err != nil {
				return err
			}

			fields = append(fields, field)
		}

		where := whereClause(fields)

		// Only produce a delete statement if there is a valid field to delete by.
		if where != "" {
			sql := fmt.Sprintf(stmts["delete"], table, where)
			s.register(buf, sql, filters...)
		}
	}
	return nil
}

func (s *Stmt) deleteRef(buf *file.Buffer) error {
	// Base snake-case name of the references (e.g. "used-by-ref" -> "used_by")
	name := strings.Replace(s.kind[len("create-"):strings.Index(s.kind, "-ref")], "-", "_", -1)

	// Field name of the reference
	fieldName := lex.Camel(name)

	// Table name where reference objects can be fetched.
	table := fmt.Sprintf("%s_%s", entityTable(s.entity), name)

	mapping, err := Parse(s.packages[s.pkg], lex.Camel(s.entity), s.kind)
	if err != nil {
		return err
	}

	field := mapping.FieldByName(fieldName)
	if field == nil {
		return fmt.Errorf("Entity %s has no field named %s", s.entity, fieldName)
	}

	where := fmt.Sprintf("%s_id = ?", s.entity)

	sql := fmt.Sprintf(stmts["delete"], table, where)
	s.register(buf, sql)

	return nil
}

// Return a where clause that filters an entity by the given fields
func whereClause(fields []*Field) string {
	via := map[string][]*Field{} // Map scalar fields to their additional indirect fields

	// Filter out indirect fields
	directFields := []*Field{}
	for _, field := range fields {
		if field.IsIndirect() {
			entity := field.Config.Get("via")
			via[entity] = append(via[entity], field)
			continue
		}
		directFields = append(directFields, field)
	}

	// If there are no valid direct fields, don't return a where clause.
	if len(directFields) == 0 {
		return ""
	}

	where := make([]string, len(directFields))

	for i, field := range directFields {
		if field.IsScalar() {
			ref := lex.Snake(field.Name)
			refTable := entityTable(ref)
			subSelect := fmt.Sprintf("SELECT %s.id FROM %s", refTable, refTable)
			for _, other := range via[ref] {
				otherRef := lex.Snake(other.Name)
				otherTable := entityTable(otherRef)
				subSelect += fmt.Sprintf(" JOIN %s ON %s.id = %s.%s_id", otherTable, otherTable, refTable, otherRef)
			}
			subSelect += fmt.Sprintf(" WHERE")
			for _, other := range via[ref] {
				otherRef := lex.Snake(other.Name)
				otherTable := entityTable(otherRef)
				subSelect += fmt.Sprintf(" %s.name = ? AND", otherTable)
			}
			subSelect += fmt.Sprintf(" %s.name = ?", refTable)
			where[i] = fmt.Sprintf("%s_id = (%s)", ref, subSelect)
		} else {

			where[i] = fmt.Sprintf("%s = ?", field.Column())
		}

	}

	return strings.Join(where, " AND ")
}

// Return a select statement that returns the ID of an entity given its natural key.
func naturalKeySelect(entity string, mapping *Mapping) string {
	nk := mapping.NaturalKey()
	criteria := ""
	for i, field := range nk {
		if i > 0 {
			criteria += " AND "
		}

		var column string
		if field.IsScalar() {
			column = field.Config.Get("join")
		} else {
			column = mapping.FieldColumnName(field.Name)
		}

		criteria += fmt.Sprintf("%s = ?", column)
	}

	table := entityTable(entity)
	for _, field := range mapping.ScalarFields() {
		join := field.Config.Get("join")
		right := strings.Split(join, ".")[0]
		via := entityTable(entity)
		if field.Config.Get("via") != "" {
			via = entityTable(field.Config.Get("via"))
		}
		table += fmt.Sprintf(" JOIN %s ON %s.%s_id = %s.id", right, via, lex.Singular(right), right)
	}

	sql := fmt.Sprintf(stmts["id"], entityTable(entity), table, criteria)

	return sql
}

// Output a line of code that registers the given statement and declares the
// associated statement code global variable.
func (s *Stmt) register(buf *file.Buffer, sql string, filters ...string) {
	kind := strings.Replace(s.kind, "-", "_", -1)
	if kind == "id" {
		kind = "ID" // silence go lints
	}
	buf.L("const %s = %s.RegisterStmt(`\n%s\n`)", stmtCodeVar(s.entity, kind, filters...), s.db, sql)
}

// Map of boilerplate statements.
var stmts = map[string]string{
	"names":   "SELECT %s\n  FROM %s\n  %sORDER BY %s",
	"objects": "SELECT %s\n  FROM %s\n  %sORDER BY %s",
	"create":  "INSERT INTO %s (%s)\n  VALUES (%s)",
	"replace": "INSERT OR REPLACE INTO %s (%s)\n VALUES (%s)",
	"id":      "SELECT %s.id FROM %s\n  WHERE %s",
	"rename":  "UPDATE %s SET name = ? WHERE %s",
	"update":  "UPDATE %s\n  SET %s\n WHERE %s",
	"delete":  "DELETE FROM %s WHERE %s",
}
