package collectors

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/go-ole/go-ole"
	"github.com/yusufpapurcu/wmi"
)

const (
	wmiNamespaceCIMv2 = `root\cimv2`
	wmiNamespaceWMI   = `root\WMI`
)

// wmiQuery runs a WQL query in a namespace under ctx. The wmi package has no
// cancellation, so a query that outlives ctx is abandoned to finish on its
// own goroutine. Null properties decode as zero values.
func wmiQuery[T any](ctx context.Context, query string, dst *[]T, namespace string) error {
	type result struct {
		rows []T
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if p := recover(); p != nil {
				r = result{err: fmt.Errorf("panic: %v", p)}
			}
			ch <- r
		}()
		c := &wmi.Client{NonePtrZero: true, AllowMissingFields: true}
		r.err = c.Query(query, &r.rows, nil, namespace)
	}()
	select {
	case r := <-ch:
		*dst = r.rows
		return wmiErrHint(r.err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WBEM status codes a non-admin service account commonly hits.
const (
	wbemAccessDenied = 0x80041003
	wbemNotSupported = 0x8004100C
	wbemInvalidClass = 0x80041010
)

// wmiErrHint names those failures. COM reports them as a generic "Exception
// occurred" whose WBEM code sits in the exception info, with a description
// that is localised, so the code is checked first and English text second.
func wmiErrHint(err error) error {
	if err == nil {
		return nil
	}
	var code uint32
	var oe *ole.OleError
	if errors.As(err, &oe) {
		code = uint32(oe.Code())
		if ei, ok := oe.SubError().(ole.EXCEPINFO); ok && ei.SCODE() != 0 {
			code = ei.SCODE()
		}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case code == wbemAccessDenied, strings.Contains(msg, "access denied"), strings.Contains(msg, "access is denied"):
		return fmt.Errorf("access denied for the service account (0x%08X): %w", code, err)
	case code == wbemNotSupported, strings.Contains(msg, "not supported"):
		return fmt.Errorf("not supported by this firmware (0x%08X): %w", code, err)
	case code == wbemInvalidClass, strings.Contains(msg, "invalid class"):
		return fmt.Errorf("class not present (0x%08X): %w", code, err)
	}
	return err
}
