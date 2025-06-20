package gaussdb

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/HuaweiCloudDeveloper/gaussdb-go"

	"github.com/HuaweiCloudDeveloper/gaussdb-go/stdlib"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

type Dialector struct {
	*Config
}

type Config struct {
	DriverName           string
	DSN                  string
	WithoutQuotingCheck  bool
	PreferSimpleProtocol bool
	WithoutReturning     bool
	Conn                 gorm.ConnPool
}

var (
	timeZoneMatcher         = regexp.MustCompile("(time_zone|TimeZone)=(.*?)($|&| )")
	defaultIdentifierLength = 63 //maximum identifier length for gaussdb
)

func Open(dsn string) gorm.Dialector {
	return &Dialector{&Config{DSN: dsn}}
}

func New(config Config) gorm.Dialector {
	return &Dialector{Config: &config}
}

func (dialector Dialector) Name() string {
	return "gaussdb"
}

func (dialector Dialector) Apply(config *gorm.Config) error {
	if config.NamingStrategy == nil {
		config.NamingStrategy = schema.NamingStrategy{
			IdentifierMaxLength: defaultIdentifierLength,
		}
		return nil
	}

	switch v := config.NamingStrategy.(type) {
	case *schema.NamingStrategy:
		if v.IdentifierMaxLength <= 0 {
			v.IdentifierMaxLength = defaultIdentifierLength
		}
	case schema.NamingStrategy:
		if v.IdentifierMaxLength <= 0 {
			v.IdentifierMaxLength = defaultIdentifierLength
			config.NamingStrategy = v
		}
	}

	return nil
}

func (dialector Dialector) Initialize(db *gorm.DB) (err error) {
	callbackConfig := &callbacks.Config{
		CreateClauses: []string{"INSERT", "VALUES", "MERGE", "ON CONFLICT"},
		UpdateClauses: []string{"UPDATE", "SET", "FROM", "WHERE"},
		DeleteClauses: []string{"DELETE", "FROM", "WHERE"},
	}
	// register callbacks
	if !dialector.WithoutReturning {
		callbackConfig.CreateClauses = append(callbackConfig.CreateClauses, "RETURNING")
		callbackConfig.UpdateClauses = append(callbackConfig.UpdateClauses, "RETURNING")
		callbackConfig.DeleteClauses = append(callbackConfig.DeleteClauses, "RETURNING")
	}
	callbacks.RegisterDefaultCallbacks(db, callbackConfig)

	if dialector.Conn != nil {
		db.ConnPool = dialector.Conn
	} else if dialector.DriverName != "" {
		db.ConnPool, err = sql.Open(dialector.DriverName, dialector.Config.DSN)
	} else {
		var config *gaussdbgo.ConnConfig

		config, err = gaussdbgo.ParseConfig(dialector.Config.DSN)
		if err != nil {
			return
		}
		if dialector.Config.PreferSimpleProtocol {
			config.DefaultQueryExecMode = gaussdbgo.QueryExecModeSimpleProtocol
		}
		result := timeZoneMatcher.FindStringSubmatch(dialector.Config.DSN)
		if len(result) > 2 {
			config.RuntimeParams["timezone"] = result[2]
		}
		db.ConnPool = stdlib.OpenDB(*config)
	}
	for k, v := range dialector.ClauseBuilders() {
		if _, ok := db.ClauseBuilders[k]; !ok {
			db.ClauseBuilders[k] = v
		}
	}
	return
}

func (dialector Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{migrator.Migrator{Config: migrator.Config{
		DB:                          db,
		Dialector:                   dialector,
		CreateIndexAfterCreateTable: true,
	}}}
}

func (dialector Dialector) DefaultValueOf(field *schema.Field) clause.Expression {
	return clause.Expr{SQL: "DEFAULT"}
}

func (dialector Dialector) BindVarTo(writer clause.Writer, stmt *gorm.Statement, v interface{}) {
	writer.WriteByte('$')
	index := 0
	varLen := len(stmt.Vars)
	if varLen > 0 {
		switch stmt.Vars[0].(type) {
		case gaussdbgo.QueryExecMode:
			index++
		}
	}
	writer.WriteString(strconv.Itoa(varLen - index))
}

func (dialector Dialector) QuoteTo(writer clause.Writer, str string) {
	if dialector.WithoutQuotingCheck {
		writer.WriteString(str)
		return
	}

	var (
		underQuoted, selfQuoted bool
		continuousBacktick      int8
		shiftDelimiter          int8
	)

	for _, v := range []byte(str) {
		switch v {
		case '"':
			continuousBacktick++
			if continuousBacktick == 2 {
				writer.WriteString(`""`)
				continuousBacktick = 0
			}
		case '.':
			if continuousBacktick > 0 || !selfQuoted {
				shiftDelimiter = 0
				underQuoted = false
				continuousBacktick = 0
				writer.WriteByte('"')
			}
			writer.WriteByte(v)
			continue
		default:
			if shiftDelimiter-continuousBacktick <= 0 && !underQuoted {
				writer.WriteByte('"')
				underQuoted = true
				if selfQuoted = continuousBacktick > 0; selfQuoted {
					continuousBacktick -= 1
				}
			}

			for ; continuousBacktick > 0; continuousBacktick -= 1 {
				writer.WriteString(`""`)
			}

			writer.WriteByte(v)
		}
		shiftDelimiter++
	}

	if continuousBacktick > 0 && !selfQuoted {
		writer.WriteString(`""`)
	}
	writer.WriteByte('"')
}

var numericPlaceholder = regexp.MustCompile(`\$(\d+)`)

func (dialector Dialector) Explain(sql string, vars ...interface{}) string {
	return logger.ExplainSQL(sql, numericPlaceholder, `'`, vars...)
}

func (dialector Dialector) DataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "boolean"
	case schema.Int, schema.Uint:
		size := field.Size
		if field.DataType == schema.Uint {
			size++
		}
		if field.AutoIncrement {
			switch {
			case size <= 16:
				return "smallserial"
			case size <= 32:
				return "serial"
			default:
				return "bigserial"
			}
		} else {
			switch {
			case size <= 16:
				return "smallint"
			case size <= 32:
				return "integer"
			default:
				return "bigint"
			}
		}
	case schema.Float:
		if field.Precision > 0 {
			if field.Scale > 0 {
				return fmt.Sprintf("numeric(%d, %d)", field.Precision, field.Scale)
			}
			return fmt.Sprintf("numeric(%d)", field.Precision)
		}
		return "decimal"
	case schema.String:
		if field.Size > 0 && field.Size <= 10485760 {
			return fmt.Sprintf("varchar(%d)", field.Size)
		}
		return "text"
	case schema.Time:
		if field.Precision > 0 {
			return fmt.Sprintf("timestamptz(%d)", field.Precision)
		}
		return "timestamptz"
	case schema.Bytes:
		return "bytea"
	default:
		return dialector.getSchemaCustomType(field)
	}
}

func (dialector Dialector) getSchemaCustomType(field *schema.Field) string {
	sqlType := string(field.DataType)

	if field.AutoIncrement && !strings.Contains(strings.ToLower(sqlType), "serial") {
		size := field.Size
		if field.GORMDataType == schema.Uint {
			size++
		}
		switch {
		case size <= 16:
			sqlType = "smallserial"
		case size <= 32:
			sqlType = "serial"
		default:
			sqlType = "bigserial"
		}
	}

	return sqlType
}

func (dialector Dialector) SavePoint(tx *gorm.DB, name string) error {
	tx.Exec("SAVEPOINT " + name)
	return nil
}

func (dialector Dialector) RollbackTo(tx *gorm.DB, name string) error {
	tx.Exec("ROLLBACK TO SAVEPOINT " + name)
	return nil
}

const (
	// ClauseOnConflict for clause.ClauseBuilder ON CONFLICT key
	ClauseOnConflict = "ON CONFLICT"
)

func (dialector Dialector) ClauseBuilders() map[string]clause.ClauseBuilder {
	clauseBuilders := map[string]clause.ClauseBuilder{
		ClauseOnConflict: func(c clause.Clause, builder clause.Builder) {
			onConflict, ok := c.Expression.(clause.OnConflict)
			if !ok {
				c.Build(builder)
				return
			}
			if len(onConflict.DoUpdates) == 0 {
				if s := builder.(*gorm.Statement).Schema; s != nil {
					var column clause.Column
					if s.PrioritizedPrimaryField != nil {
						column = clause.Column{Name: s.PrioritizedPrimaryField.DBName}
					} else if len(s.DBNames) > 0 {
						column = clause.Column{Name: s.DBNames[0]}
					}
					if column.Name != "" && !isPrimaryOrUniqueKey(builder, column.Name) {
						onConflict.DoUpdates = []clause.Assignment{{Column: column, Value: column}}
						onConflict.DoNothing = false
					}
					builder.(*gorm.Statement).AddClause(onConflict)
				}
			}

			notFirstField := false
			hasWritten := false
			for _, assignment := range onConflict.DoUpdates {
				if isPrimaryOrUniqueKey(builder, assignment.Column.Name) {
					continue
				}
				if !hasWritten {
					builder.WriteString("ON DUPLICATE KEY UPDATE ")
					hasWritten = true
				}
				if notFirstField {
					builder.WriteByte(',')
				}
				builder.WriteQuoted(assignment.Column)
				builder.WriteByte('=')
				if column, ok := assignment.Value.(clause.Column); ok && column.Table == "excluded" {
					column.Table = ""
					builder.WriteString("VALUES(")
					builder.WriteQuoted(column)
					builder.WriteByte(')')
				} else {
					builder.AddVar(builder, assignment.Value)
				}
				notFirstField = true
			}
			if !hasWritten {
				builder.WriteString("ON DUPLICATE KEY UPDATE NOTHING")
			}
		},
	}

	return clauseBuilders
}
func getSerialDatabaseType(s string) (dbType string, ok bool) {
	switch s {
	case "smallserial":
		return "smallint", true
	case "serial":
		return "integer", true
	case "bigserial":
		return "bigint", true
	default:
		return "", false
	}
}

func isPrimaryOrUniqueKey(builder clause.Builder, name string) bool {
	s := builder.(*gorm.Statement).Schema
	if s == nil || name == "" {
		return false
	}
	if s.PrimaryFields != nil {
		for _, field := range s.PrimaryFields {
			if field.DBName == name && (field.PrimaryKey || field.Unique) {
				return true
			}
		}
	}
	return false
}
