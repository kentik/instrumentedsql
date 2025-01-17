// +build go1.9

package instrumentedsql

import "database/sql/driver"

var (
	_ driver.NamedValueChecker = WrappedConn{}
)

func (c WrappedConn) CheckNamedValue(v *driver.NamedValue) error {
	if checker, ok := c.Parent.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(v)
	}

	return driver.ErrSkip
}
