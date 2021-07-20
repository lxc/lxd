package db

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/lxc/lxd/lxd/db/generate/file"
	"github.com/lxc/lxd/lxd/db/generate/lex"
	"github.com/pkg/errors"
)

// Method generates a code snippet for a particular database query method.
type Method struct {
	db       string                  // Target database (cluster or node)
	pkg      string                  // Package where the entity struct is declared.
	entity   string                  // Name of the database entity
	kind     string                  // Kind of statement to generate
	config   map[string]string       // Configuration parameters
	packages map[string]*ast.Package // Packages to perform for struct declaration lookups
}

// NewMethod return a new method code snippet for executing a certain mapping.
func NewMethod(database, pkg, entity, kind string, config map[string]string) (*Method, error) {
	packages, err := Packages()
	if err != nil {
		return nil, err
	}

	method := &Method{
		db:       database,
		pkg:      pkg,
		entity:   entity,
		kind:     kind,
		config:   config,
		packages: packages,
	}

	return method, nil
}

// Generate the desired method.
func (m *Method) Generate(buf *file.Buffer) error {
	if strings.HasSuffix(m.kind, "Ref") {
		return m.ref(buf)
	}
	switch m.kind {
	case "URIs":
		return m.uris(buf)
	case "List":
		return m.list(buf)
	case "Get":
		return m.get(buf)
	case "ID":
		return m.id(buf)
	case "Exists":
		return m.exists(buf)
	case "Create":
		return m.create(buf, false)
	case "CreateOrReplace":
		return m.create(buf, true)
	case "Rename":
		return m.rename(buf)
	case "Update":
		return m.update(buf)
	case "DeleteOne":
		return m.delete(buf, true)
	case "DeleteMany":
		return m.delete(buf, false)
	default:
		return fmt.Errorf("Unknown method kind '%s'", m.kind)
	}
}

func (m *Method) uris(buf *file.Buffer) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	criteria, err := Criteria(m.packages["db"], m.entity)
	if err != nil {
		return errors.Wrap(err, "Parse filter struct")
	}

	filters := Filters(m.packages["db"], "objects", m.entity)

	comment := fmt.Sprintf("returns all available %s URIs.", m.entity)
	args := fmt.Sprintf("filter %s", entityFilter(m.entity))
	rets := "([]string, error)"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	buf.L("// Check which filter criteria are active.")
	buf.L("criteria := map[string]interface{}{}")

	for _, name := range criteria {
		if name == "Parent" {
			continue
		}
		field, err := mapping.FilterFieldByName(name)
		if err != nil {
			return err
		}
		buf.L("if filter.%s != %s {", name, field.ZeroValue())
		buf.L("        criteria[%q] = filter.%s", name, name)
		buf.L("}")
	}

	buf.N()
	buf.L("// Pick the prepared statement and arguments to use based on active criteria.")
	buf.L("var stmt *sql.Stmt")
	buf.L("var args []interface{}")
	buf.N()

	for i, filter := range filters {
		branch := "if"
		if i > 0 {
			branch = "} else if"
		}
		buf.L("%s %s {", branch, activeCriteria(filter))

		buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "names", filter...))
		buf.L("args = []interface{}{")

		for _, name := range filter {
			buf.L("filter.%s,", name)
		}

		buf.L("}")

		// Last branch, no filter to use.
		if i == len(filters)-1 {
			buf.L("} else {")
		}
	}
	// Else branch.
	buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "names"))
	buf.L("args = []interface{}{}")
	buf.L("}")
	buf.N()

	buf.L("code := %s.EntityTypes[%q]", m.db, m.entity)
	buf.L("formatter := %s.EntityFormatURIs[code]", m.db)
	buf.N()
	buf.L("return query.SelectURIs(stmt, formatter, args...)")

	return nil
}

func (m *Method) list(buf *file.Buffer) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	criteria, err := Criteria(m.packages["db"], m.entity)
	if err != nil {
		return errors.Wrap(err, "Parse filter struct")
	}

	filters := Filters(m.packages["db"], "objects", m.entity)

	// Go type name the objects to return (e.g. api.Foo).
	typ := entityType(m.pkg, m.entity)

	comment := fmt.Sprintf("returns all available %s.", lex.Plural(m.entity))
	args := fmt.Sprintf("filter %s", entityFilter(m.entity))
	rets := fmt.Sprintf("(%s, error)", lex.Slice(typ))

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	buf.L("// Result slice.")
	buf.L("objects := make(%s, 0)", lex.Slice(typ))
	buf.N()
	buf.L("// Check which filter criteria are active.")
	buf.L("criteria := map[string]interface{}{}")

	for _, name := range criteria {
		var zero string
		if name == "Parent" {
			zero = `""`
		} else {
			field, err := mapping.FilterFieldByName(name)
			if err != nil {
				return err
			}
			zero = field.ZeroValue()
		}
		buf.L("if filter.%s != %s {", name, zero)
		buf.L("        criteria[%q] = filter.%s", name, name)
		buf.L("}")
	}

	buf.N()
	buf.L("// Pick the prepared statement and arguments to use based on active criteria.")
	buf.L("var stmt *sql.Stmt")
	buf.L("var args []interface{}")
	buf.N()

	for i, filter := range filters {
		branch := "if"
		if i > 0 {
			branch = "} else if"
		}
		buf.L("%s %s {", branch, activeCriteria(filter))

		buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "objects", filter...))
		buf.L("args = []interface{}{")

		for _, name := range filter {
			if name == "Parent" {
				buf.L("len(filter.Parent)+1,")
				buf.L("filter.%s+\"/\",", name)
			} else {
				buf.L("filter.%s,", name)
			}
		}

		buf.L("}")

		// Last branch, no filter to use.
		if i == len(filters)-1 {
			buf.L("} else {")
		}
	}
	// Else branch.
	buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "objects"))
	buf.L("args = []interface{}{}")
	buf.L("}")

	buf.N()
	buf.L("// Dest function for scanning a row.")
	buf.L("dest := %s", destFunc("objects", typ, mapping.ColumnFields()))
	buf.N()
	buf.L("// Select.")
	buf.L("err := query.SelectObjects(stmt, dest, args...)")
	buf.L("if err != nil {")
	buf.L("        return nil, errors.Wrap(err, \"Failed to fetch %s\")", lex.Plural(m.entity))
	buf.L("}")
	buf.N()

	// Fill reference fields.
	nk := mapping.NaturalKey()
	for _, field := range mapping.RefFields() {
		err := m.fillSliceReferenceField(buf, nk, field)
		if err != nil {
			return err
		}
	}

	buf.L("return objects, nil")

	return nil
}

func (m *Method) get(buf *file.Buffer) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	comment := fmt.Sprintf("returns the %s with the given key.", m.entity)

	nk := mapping.NaturalKey()
	typ := entityType(m.pkg, m.entity)

	args := FieldArgs(nk)
	rets := fmt.Sprintf("(%s, error)", lex.Star(typ))

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	buf.L("filter := %s{}", entityFilter(m.entity))
	for _, field := range nk {
		buf.L("filter.%s = %s", field.Name, lex.Minuscule(field.Name))
	}
	// FIXME: snowflake
	if m.entity == "instance" {
		buf.L("filter.Type = -1")
	}
	buf.N()
	buf.L("objects, err := c.Get%s(filter)", lex.Plural(lex.Camel(m.entity)))
	buf.L("if err != nil {")
	buf.L("        return nil, errors.Wrap(err, \"Failed to fetch %s\")", lex.Camel(m.entity))
	buf.L("}")
	buf.N()
	buf.L("switch len(objects) {")
	buf.L("case 0:")
	buf.L("        return nil, ErrNoSuchObject")
	buf.L("case 1:")
	buf.L("        return &objects[0], nil")
	buf.L("default:")
	buf.L("        return nil, fmt.Errorf(\"More than one %s matches\")", m.entity)
	buf.L("}")

	return nil
}

func (m *Method) ref(buf *file.Buffer) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	nk := mapping.NaturalKey()

	name := m.kind[:len(m.kind)-len("Ref")]
	field := mapping.FieldByName(name)

	var typ string
	var retTyp string
	var destType string
	var destFields []*Field

	if field.Type.Code == TypeSlice {
		retTyp = field.Type.Name
		typ = lex.Element(field.Type.Name)
		if IsColumnType(typ) {
			destType = "struct {\n"
			for _, field := range nk {
				destType += fmt.Sprintf("%s %s\n", field.Name, field.Type.Name)
			}
			destType += fmt.Sprintf("Value %s\n}", typ)
			valueField := Field{Name: "Value"}
			destFields = append(nk, &valueField)
		} else {
			// TODO
			destType = typ
			destFields = nil
		}
	} else if field.Type.Code == TypeMap && field.Type.Name == "map[string]string" {
		retTyp = field.Type.Name
		// Config reference
		destType = "struct {\n"
		for _, field := range nk {
			destType += fmt.Sprintf("%s %s\n", field.Name, field.Type.Name)
		}
		destType += fmt.Sprintf("Key string\n")
		destType += fmt.Sprintf("Value string\n}")
		keyField := Field{Name: "Key"}
		valueField := Field{Name: "Value"}
		destFields = append(nk, &keyField, &valueField)
	} else if field.Type.Code == TypeMap && field.Type.Name == "map[string]map[string]string" {
		retTyp = field.Type.Name
		// Device reference
		destType = "struct {\n"
		for _, field := range nk {
			destType += fmt.Sprintf("%s %s\n", field.Name, field.Type.Name)
		}
		destType += fmt.Sprintf("Device string\n")
		destType += fmt.Sprintf("Type int\n")
		destType += fmt.Sprintf("Key string\n")
		destType += fmt.Sprintf("Value string\n}")
		deviceField := Field{Name: "Device"}
		typeField := Field{Name: "Type"}
		keyField := Field{Name: "Key"}
		valueField := Field{Name: "Value"}
		destFields = append(nk, &deviceField, &typeField, &keyField, &valueField)

	} else {
		return fmt.Errorf("Unsupported ref type %q", field.Type.Name)
	}

	comment := fmt.Sprintf("returns entities used by %s.", lex.Plural(m.entity))

	// The type of the returned index takes into account composite natural
	// keys.
	indexTyp := indexType(nk, retTyp)

	args := fmt.Sprintf("filter %s", entityFilter(m.entity))
	rets := fmt.Sprintf("(%s, error)", indexTyp)

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	criteria, err := Criteria(m.packages["db"], m.entity)
	if err != nil {
		return errors.Wrap(err, "Parse filter struct")
	}

	filters := RefFilters(m.packages["db"], m.entity, name)

	buf.L("// Result slice.")
	buf.L("objects := make(%s, 0)", lex.Slice(destType))
	buf.N()
	buf.L("// Check which filter criteria are active.")
	buf.L("criteria := map[string]interface{}{}")

	for _, name := range criteria {
		var zero string
		if name == "Parent" {
			zero = `""`
		} else {
			field := mapping.FieldByName(name)
			if !field.IsPrimary() {
				continue
			}
			zero = field.ZeroValue()
		}
		buf.L("if filter.%s != %s {", name, zero)
		buf.L("        criteria[%q] = filter.%s", name, name)
		buf.L("}")
	}

	buf.N()
	buf.L("// Pick the prepared statement and arguments to use based on active criteria.")
	buf.L("var stmt *sql.Stmt")
	buf.L("var args []interface{}")
	buf.N()

	for i, filter := range filters {
		branch := "if"
		if i > 0 {
			branch = "} else if"
		}
		buf.L("%s %s {", branch, activeCriteria(filter))

		buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, m.kind, filter...))
		buf.L("args = []interface{}{")

		for _, name := range filter {
			buf.L("filter.%s,", name)
		}

		buf.L("}")

		// Last branch, no filter to use.
		if i == len(filters)-1 {
			buf.L("} else {")
		}
	}

	// Else branch.
	buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, m.kind))
	buf.L("args = []interface{}{}")
	buf.L("}")
	buf.N()
	buf.L("// Dest function for scanning a row.")
	buf.L("dest := %s", destFunc("objects", destType, destFields))
	buf.N()
	buf.L("// Select.")
	buf.L("err := query.SelectObjects(stmt, dest, args...)")
	buf.L("if err != nil {")
	buf.L("        return nil, errors.Wrap(err, \"Failed to fetch %s ref for %s\")", typ, lex.Plural(m.entity))
	buf.L("}")
	buf.N()

	buf.L("// Build index by primary name.")
	buf.L("index := %s{}", indexTyp)
	buf.N()
	buf.L("for _, object := range objects {")
	needle := ""
	for i, key := range nk[:len(nk)-1] {
		needle += fmt.Sprintf("[object.%s]", key.Name)

		subIndexTyp := indexType(nk[i+1:], retTyp)
		buf.L("        _, ok%d := index%s", i, needle)
		buf.L("        if !ok%d {", i)
		buf.L("                subIndex := %s{}", subIndexTyp)
		buf.L("                index%s = subIndex", needle)
		buf.L("        }")
		buf.N()
	}

	needle += fmt.Sprintf("[object.%s]", nk[len(nk)-1].Name)
	buf.L("        item, ok := index%s", needle)
	buf.L("        if !ok {")
	buf.L("                item = %s{}", retTyp)
	buf.L("        }")
	buf.N()
	if field.Type.Code == TypeSlice && IsColumnType(typ) {
		buf.L("        index%s = append(item, object.Value)", needle)
	} else if field.Type.Code == TypeMap && field.Type.Name == "map[string]string" {
		buf.L("        index%s = item", needle)
		buf.L("        item[object.Key] = object.Value")
	} else if field.Type.Code == TypeMap && field.Type.Name == "map[string]map[string]string" {
		buf.L("        index%s = item", needle)
		buf.L("        config, ok := item[object.Device]")
		buf.L("        if !ok {")
		buf.L("                // First time we see this device, let's int the config")
		buf.L("                // and add the type.")
		buf.L("                deviceType, err := deviceTypeToString(object.Type)")
		buf.L("                if err != nil {")
		buf.L("                        return nil, errors.Wrapf(")
		buf.L("                            err, \"unexpected device type code '%%d'\", object.Type)")
		buf.L("                }")
		buf.L("                config = map[string]string{}")
		buf.L("                config[\"type\"] = deviceType")
		buf.L("                item[object.Device] = config")
		buf.L("        }")
		buf.L("        if object.Key != \"\" {")
		buf.L("                config[object.Key] = object.Value")
		buf.L("        }")
	} else {
	}
	buf.L("}")
	buf.N()
	buf.L("return index, nil")

	return nil
}

// Populate a field consisting of a slice of objects referencing the
// entity. This information is available by joining a the view or table
// associated with the type of the referenced objects, which must contain the
// natural key of the entity.
func (m *Method) fillSliceReferenceField(buf *file.Buffer, nk []*Field, field *Field) error {
	objectsVar := fmt.Sprintf("%sObjects", lex.Minuscule(field.Name))
	methodName := fmt.Sprintf("%s%sRef", lex.Camel(m.entity), field.Name)

	buf.L("// Fill field %s.", field.Name)
	buf.L("%s, err := c.%s(filter)", objectsVar, methodName)
	buf.L("if err != nil {")
	buf.L("        return nil, errors.Wrap(err, \"Failed to fetch field %s\")", field.Name)
	buf.L("}")
	buf.N()
	buf.L("for i := range objects {")
	needle := ""
	for i, key := range nk[:len(nk)-1] {
		needle += fmt.Sprintf("[objects[i].%s]", key.Name)
		subIndexTyp := indexType(nk[i+1:], field.Type.Name)
		buf.L("        _, ok%d := %s%s", i, objectsVar, needle)
		buf.L("        if !ok%d {", i)
		buf.L("                subIndex := %s{}", subIndexTyp)
		buf.L("                %s%s = subIndex", objectsVar, needle)
		buf.L("        }")
		buf.N()
	}

	needle += fmt.Sprintf("[objects[i].%s]", nk[len(nk)-1].Name)
	buf.L("        value := %s%s", objectsVar, needle)
	buf.L("        if value == nil {")
	buf.L("                value = %s{}", field.Type.Name)
	buf.L("        }")
	if field.Name == "UsedBy" {
		buf.L("        for j := range value {")
		buf.L("                if len(value[j]) > 12 && value[j][len(value[j])-12:] == \"&target=none\" {")
		buf.L("                         value[j] = value[j][0:len(value[j])-12]")
		buf.L("                }")
		buf.L("                if len(value[j]) > 16 && value[j][len(value[j])-16:] == \"?project=default\" {")
		buf.L("                         value[j] = value[j][0:len(value[j])-16]")
		buf.L("                }")
		buf.L("        }")
	}
	buf.L("        objects[i].%s = value", field.Name)
	buf.L("}")
	buf.N()

	return nil
}

func (m *Method) id(buf *file.Buffer) error {
	// Support using a different structure or package to pass arguments to Create.
	entityCreate, ok := m.config["struct"]
	if !ok {
		entityCreate = entityPost(m.entity)
	}

	mapping, err := Parse(m.packages[m.pkg], entityCreate, m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	nk := mapping.NaturalKey()

	comment := fmt.Sprintf("return the ID of the %s with the given key.", m.entity)
	args := FieldArgs(nk)
	rets := "(int64, error)"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	buf.L("stmt := c.stmt(%s)", stmtCodeVar(m.entity, "ID"))
	buf.L("rows, err := stmt.Query(%s)", FieldParams(nk))
	buf.L("if err != nil {")
	buf.L("        return -1, errors.Wrap(err, \"Failed to get %s ID\")", m.entity)
	buf.L("}")
	buf.L("defer rows.Close()")
	buf.N()
	buf.L("// Ensure we read one and only one row.")
	buf.L("if !rows.Next() {")
	buf.L("        return -1, ErrNoSuchObject")
	buf.L("}")
	buf.L("var id int64")
	buf.L("err = rows.Scan(&id)")
	buf.L("if err != nil {")
	buf.L("        return -1, errors.Wrap(err, \"Failed to scan ID\")")
	buf.L("}")
	buf.L("if rows.Next() {")
	buf.L("        return -1, fmt.Errorf(\"More than one row returned\")")
	buf.L("}")
	buf.L("err = rows.Err()")
	buf.L("if err != nil {")
	buf.L("        return -1, errors.Wrap(err, \"Result set failure\")")
	buf.L("}")
	buf.N()
	buf.L("return id, nil")

	return nil
}

func (m *Method) exists(buf *file.Buffer) error {
	// Support using a different structure or package to pass arguments to Create.
	entityCreate, ok := m.config["struct"]
	if !ok {
		entityCreate = entityPost(m.entity)
	}

	mapping, err := Parse(m.packages[m.pkg], entityCreate, m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	nk := mapping.NaturalKey()

	comment := fmt.Sprintf("checks if a %s with the given key exists.", m.entity)
	args := FieldArgs(nk)
	rets := "(bool, error)"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	buf.L("_, err := c.Get%sID(%s)", lex.Camel(m.entity), FieldParams(nk))
	buf.L("if err != nil {")
	buf.L("        if err == ErrNoSuchObject {")
	buf.L("                return false, nil")
	buf.L("        }")
	buf.L("        return false, err")
	buf.L("}")
	buf.N()
	buf.L("return true, nil")

	return nil
}

func (m *Method) create(buf *file.Buffer, replace bool) error {
	// Support using a different structure or package to pass arguments to Create.
	entityCreate, ok := m.config["struct"]
	if !ok {
		entityCreate = entityPost(m.entity)
	}

	mapping, err := Parse(m.packages[m.pkg], entityCreate, m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	comment := fmt.Sprintf("adds a new %s to the database.", m.entity)

	typ := entityType(m.pkg, entityCreate)

	args := fmt.Sprintf("object %s", typ)
	rets := "(int64, error)"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	nk := mapping.NaturalKey()
	nkParams := make([]string, len(nk))
	for i, field := range nk {
		nkParams[i] = fmt.Sprintf("object.%s", field.Name)
	}

	kind := "create"
	if replace {
		kind = "create_or_replace"
	} else {
		buf.L("// Check if a %s with the same key exists.", m.entity)
		buf.L("exists, err := c.%sExists(%s)", lex.Camel(m.entity), strings.Join(nkParams, ", "))
		buf.L("if err != nil {")
		buf.L("        return -1, errors.Wrap(err, \"Failed to check for duplicates\")")
		buf.L("}")
		buf.L("if exists {")
		buf.L("        return -1, fmt.Errorf(\"This %s already exists\")", m.entity)
		buf.L("}")
		buf.N()
	}

	fields := mapping.ColumnFields("ID")
	buf.L("args := make([]interface{}, %d)", len(fields))
	buf.N()

	buf.L("// Populate the statement arguments. ")
	for i, field := range fields {
		buf.L("args[%d] = object.%s", i, field.Name)
	}

	buf.N()

	buf.L("// Prepared statement to use. ")
	buf.L("stmt := c.stmt(%s)", stmtCodeVar(m.entity, kind))
	buf.N()
	buf.L("// Execute the statement. ")
	buf.L("result, err := stmt.Exec(args...)")
	buf.L("if err != nil {")
	buf.L("        return -1, errors.Wrap(err, \"Failed to create %s\")", m.entity)
	buf.L("}")
	buf.N()
	buf.L("id, err := result.LastInsertId()")
	buf.L("if err != nil {")
	buf.L("        return -1, errors.Wrap(err, \"Failed to fetch %s ID\")", m.entity)
	buf.L("}")
	buf.N()

	fields = mapping.RefFields()
	for _, field := range fields {
		if field.Type.Name == "map[string]string" {
			buf.L("// Insert config reference. ")
			buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "createConfigRef"))
			buf.L("for key, value := range object.%s {", field.Name)
			buf.L("        _, err := stmt.Exec(id, key, value)")
			buf.L("        if err != nil {")
			buf.L("                return -1, errors.Wrap(err, \"Insert config for %s\")", m.entity)
			buf.L("        }")
			buf.L("}")
			buf.N()
		}
		if field.Type.Name == "map[string]map[string]string" {
			buf.L("// Insert devices reference. ")
			buf.L("for name, config := range object.%s {", field.Name)
			buf.L("        typ, ok := config[\"type\"]")
			buf.L("        if !ok {")
			buf.L("                return -1, fmt.Errorf(\"No type for device %%s\", name)")
			buf.L("        }")
			buf.L("        typCode, err := deviceTypeToInt(typ)")
			buf.L("        if err != nil {")
			buf.L("                return -1, errors.Wrapf(err, \"Device type code for %%s\", typ)")
			buf.L("        }")
			buf.L("        stmt = c.stmt(%s)", stmtCodeVar(m.entity, "createDevicesRef"))
			buf.L("        result, err := stmt.Exec(id, name, typCode)")
			buf.L("        if err != nil {")
			buf.L("                return -1, errors.Wrapf(err, \"Insert device %%s\", name)")
			buf.L("        }")
			buf.L("        deviceID, err := result.LastInsertId()")
			buf.L("        if err != nil {")
			buf.L("                return -1, errors.Wrap(err, \"Failed to fetch device ID\")")
			buf.L("        }")
			buf.L("        stmt = c.stmt(%s)", stmtCodeVar(m.entity, "createDevicesConfigRef"))
			buf.L("        for key, value := range config {")
			buf.L("                _, err := stmt.Exec(deviceID, key, value)")
			buf.L("                if err != nil {")
			buf.L("                        return -1, errors.Wrap(err, \"Insert config for %s\")", m.entity)
			buf.L("                }")
			buf.L("        }")
			buf.L("}")
			buf.N()
		}
		if field.Name == "Profiles" {
			// TODO: get rid of the special case
			buf.L("// Insert profiles reference. ")
			buf.L("err = addProfilesToInstance(c.tx, int(id), object.Project, object.Profiles)")
			buf.L("if err != nil {")
			buf.L("        return -1, errors.Wrap(err, \"Insert profiles for %s\")", m.entity)
			buf.L("}")
		}
	}

	buf.L("return id, nil")

	return nil
}

func (m *Method) rename(buf *file.Buffer) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	nk := mapping.NaturalKey()

	comment := fmt.Sprintf("renames the %s matching the given key parameters.", m.entity)
	args := FieldArgs(nk) + ", to string"
	rets := "error"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	buf.L("stmt := c.stmt(%s)", stmtCodeVar(m.entity, "rename"))
	buf.L("result, err := stmt.Exec(%s)", "to, "+FieldParams(nk))
	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Rename %s\")", m.entity)
	buf.L("}")
	buf.N()
	buf.L("n, err := result.RowsAffected()")
	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Fetch affected rows\")")
	buf.L("}")
	buf.L("if n != 1 {")
	buf.L("        return fmt.Errorf(\"Query affected %%d rows instead of 1\", n)")
	buf.L("}")

	buf.L("return nil")

	return nil
}

func (m *Method) update(buf *file.Buffer) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	// Support using a different structure or package to pass arguments to Create.
	entityUpdate, ok := m.config["struct"]
	if !ok {
		entityUpdate = entityPut(m.entity)
	}

	nk := mapping.NaturalKey()

	comment := fmt.Sprintf("updates the %s matching the given key parameters.", m.entity)
	args := FieldArgs(nk) + fmt.Sprintf(", object %s", entityType(m.pkg, entityUpdate))
	rets := "error"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)

	updateMapping, err := Parse(m.packages[m.pkg], entityUpdate, m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}
	fields := updateMapping.ColumnFields("ID") // This exclude the ID column, which is autogenerated.

	params := make([]string, len(fields))

	for i, field := range fields {
		params[i] = fmt.Sprintf("object.%s", field.Name)
	}

	//buf.L("id, err := c.Get%s(%s)", lex.Camel(m.entity), FieldArgs(nk))
	buf.L("id, err := c.Get%sID(%s)", lex.Camel(m.entity), FieldParams(nk))
	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Get %s\")", m.entity)
	buf.L("}")
	buf.N()
	buf.L("stmt := c.stmt(%s)", stmtCodeVar(m.entity, "update"))
	buf.L("result, err := stmt.Exec(%s)", strings.Join(params, ", ")+", id")
	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Update %s\")", m.entity)
	buf.L("}")
	buf.N()
	buf.L("n, err := result.RowsAffected()")
	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Fetch affected rows\")")
	buf.L("}")
	buf.L("if n != 1 {")
	buf.L("        return fmt.Errorf(\"Query updated %%d rows instead of 1\", n)")
	buf.L("}")
	buf.N()

	fields = mapping.RefFields()
	for _, field := range fields {
		switch field.Name {
		case "Config":
			buf.L("// Delete current config. ")
			buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "deleteConfigRef"))
			buf.L("_, err = stmt.Exec(id)")
			buf.L("if err != nil {")
			buf.L("        return errors.Wrap(err, \"Delete current config\")")
			buf.L("}")
			buf.N()
			buf.L("// Insert config reference. ")
			buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "createConfigRef"))
			buf.L("for key, value := range object.%s {", field.Name)
			buf.L("        if value == \"\" {")
			buf.L("                continue")
			buf.L("        }")
			buf.L("        _, err := stmt.Exec(id, key, value)")
			buf.L("        if err != nil {")
			buf.L("                return errors.Wrap(err, \"Insert config for %s\")", m.entity)
			buf.L("        }")
			buf.L("}")
			buf.N()
		case "Devices":
			buf.L("// Delete current devices. ")
			buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "deleteDevicesRef"))
			buf.L("_, err = stmt.Exec(id)")
			buf.L("if err != nil {")
			buf.L("        return errors.Wrap(err, \"Delete current devices\")")
			buf.L("}")
			buf.N()
			buf.L("// Insert devices reference. ")
			buf.L("for name, config := range object.%s {", field.Name)
			buf.L("        typ, ok := config[\"type\"]")
			buf.L("        if !ok {")
			buf.L("                return fmt.Errorf(\"No type for device %%s\", name)")
			buf.L("        }")
			buf.L("        typCode, err := deviceTypeToInt(typ)")
			buf.L("        if err != nil {")
			buf.L("                return errors.Wrapf(err, \"Device type code for %%s\", typ)")
			buf.L("        }")
			buf.L("        stmt = c.stmt(%s)", stmtCodeVar(m.entity, "createDevicesRef"))
			buf.L("        result, err := stmt.Exec(id, name, typCode)")
			buf.L("        if err != nil {")
			buf.L("                return errors.Wrapf(err, \"Insert device %%s\", name)")
			buf.L("        }")
			buf.L("        deviceID, err := result.LastInsertId()")
			buf.L("        if err != nil {")
			buf.L("                return errors.Wrap(err, \"Failed to fetch device ID\")")
			buf.L("        }")
			buf.L("        stmt = c.stmt(%s)", stmtCodeVar(m.entity, "createDevicesConfigRef"))
			buf.L("        for key, value := range config {")
			buf.L("                if value == \"\" {")
			buf.L("                        continue")
			buf.L("                }")
			buf.L("                _, err := stmt.Exec(deviceID, key, value)")
			buf.L("                if err != nil {")
			buf.L("                        return errors.Wrap(err, \"Insert config for %s\")", m.entity)
			buf.L("                }")
			buf.L("        }")
			buf.L("}")
			buf.N()
		case "Profiles":
			buf.L("// Delete current profiles. ")
			buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "deleteProfilesRef"))
			buf.L("_, err = stmt.Exec(id)")
			buf.L("if err != nil {")
			buf.L("        return errors.Wrap(err, \"Delete current profiles\")")
			buf.L("}")
			buf.N()
			buf.L("// Insert profiles reference. ")
			buf.L("err = addProfilesToInstance(c.tx, int(id), object.Project, object.Profiles)")
			buf.L("if err != nil {")
			buf.L("        return errors.Wrap(err, \"Insert profiles for %s\")", m.entity)
			buf.L("}")
		}
	}

	buf.L("return nil")

	return nil
}

func (m *Method) delete(buf *file.Buffer, deleteOne bool) error {
	mapping, err := Parse(m.packages[m.pkg], lex.Camel(m.entity), m.kind)
	if err != nil {
		return errors.Wrap(err, "Parse entity struct")
	}

	criteria, err := Criteria(m.packages["db"], m.entity)
	if err != nil {
		return errors.Wrap(err, "Parse filter struct")
	}

	filters := Filters(m.packages["db"], "delete", m.entity)

	comment := fmt.Sprintf("deletes the %s matching the given key parameters.", m.entity)
	args := fmt.Sprintf("filter %s", entityFilter(m.entity))
	rets := "error"

	m.begin(buf, comment, args, rets)
	defer m.end(buf)
	buf.L("// Check which filter criteria are active.")
	buf.L("criteria := map[string]interface{}{}")

	for _, name := range criteria {
		var zero string
		if name == "Parent" {
			zero = `""`
		} else {
			field, err := mapping.FilterFieldByName(name)
			if err != nil {
				return err
			}
			zero = field.ZeroValue()
		}
		buf.L("if filter.%s != %s {", name, zero)
		buf.L("        criteria[%q] = filter.%s", name, name)
		buf.L("}")
	}

	buf.N()
	buf.L("// Pick the prepared statement and arguments to use based on active criteria.")
	buf.L("var stmt *sql.Stmt")
	buf.L("var args []interface{}")
	buf.N()

	for i, filter := range filters {
		branch := "if"
		if i > 0 {
			branch = "} else if"
		}
		buf.L("%s %s {", branch, activeCriteria(filter))

		buf.L("stmt = c.stmt(%s)", stmtCodeVar(m.entity, "delete", filter...))
		buf.L("args = []interface{}{")

		for _, name := range filter {
			if name == "Parent" {
				buf.L("len(filter.Parent)+1,")
				buf.L("filter.%s+\"/\",", name)
			} else {
				buf.L("filter.%s,", name)
			}
		}

		buf.L("}")

		// Last branch, no filter to use.
		if i == len(filters)-1 {
			buf.L("} else {")
		}
	}
	// Else branch.
	buf.L("return fmt.Errorf(\"No valid filter for %s delete\")", m.entity)
	buf.L("}")

	buf.L("result, err := stmt.Exec(args...)")
	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Delete %s\")", m.entity)
	buf.L("}")
	buf.N()

	if deleteOne {
		buf.L("n, err := result.RowsAffected()")
	} else {
		buf.L("_, err = result.RowsAffected()")
	}

	buf.L("if err != nil {")
	buf.L("        return errors.Wrap(err, \"Fetch affected rows\")")
	buf.L("}")

	if deleteOne {
		buf.L("if n != 1 {")
		buf.L("        return fmt.Errorf(\"Query deleted %%d rows instead of 1\", n)")
		buf.L("}")
	}

	buf.N()
	buf.L("return nil")
	return nil
}

func (m *Method) begin(buf *file.Buffer, comment string, args string, rets string) {
	name := ""
	entity := lex.Camel(m.entity)
	switch m.kind {
	case "URIs":
		name = fmt.Sprintf("Get%sURIs", entity)
	case "List":
		name = fmt.Sprintf("Get%s", lex.Plural(entity))
	case "Get":
		name = fmt.Sprintf("Get%s", entity)
	case "ID":
		name = fmt.Sprintf("Get%sID", entity)
	case "Exists":
		name = fmt.Sprintf("%sExists", entity)
	case "Create":
		name = fmt.Sprintf("Create%s", entity)
	case "CreateOrReplace":
		name = fmt.Sprintf("CreateOrReplace%s", entity)
	case "Rename":
		name = fmt.Sprintf("Rename%s", entity)
	case "Update":
		name = fmt.Sprintf("Update%s", entity)
	case "DeleteOne":
		name = fmt.Sprintf("Delete%s", entity)
	case "DeleteMany":
		name = fmt.Sprintf("Delete%ss", entity)
	default:
		name = fmt.Sprintf("%s%s", entity, m.kind)
	}
	receiver := fmt.Sprintf("c %s", dbTxType(m.db))

	buf.L("// %s %s", name, comment)
	buf.L("func (%s) %s(%s) %s {", receiver, name, args, rets)
}

func (m *Method) end(buf *file.Buffer) {
	buf.L("}")
}
