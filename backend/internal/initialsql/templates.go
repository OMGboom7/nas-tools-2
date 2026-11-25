// Package initialsql contains the original NAS Tools preset data used at
// first start and by the filter template editor.
package initialsql

import _ "embed"

//go:embed init_filter.sql
var Filters string

//go:embed init_userrss_v3.sql
var RSSParsers string
