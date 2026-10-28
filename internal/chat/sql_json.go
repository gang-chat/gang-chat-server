package chat

// jsonObjectField is the SQL for one field of a json_each() row, or NULL when
// that element is not an object (a malformed element would otherwise make the
// whole query fail). It stands in for MySQL's JSON_TABLE(... COLUMNS (x PATH
// '$.field')): `FROM json_each(m.attachments_json) a WHERE <field> = ?`.
func jsonObjectField(alias, path string) string {
	return "CASE WHEN " + alias + ".type = 'object' THEN " + alias + ".value ->> '" + path + "' END"
}
