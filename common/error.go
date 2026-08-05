package common

import (
	"fmt"
)

type Error struct {
	info string
	// base holds the first wrapped error so callers can traverse the chain
	// with errors.Is / errors.As. Without this the error is flattened into
	// a string and type information (e.g. net.Error timeouts) is lost.
	base error
}

func (e *Error) Error() string {
	return e.info
}

// Unwrap exposes the underlying error for errors.Is / errors.As.
func (e *Error) Unwrap() error {
	return e.base
}

func (e *Error) Base(err error) *Error {
	if err != nil {
		if e.base == nil {
			e.base = err
		}
		e.info += " | " + err.Error()
	}
	return e
}

func NewError(info string) *Error {
	return &Error{
		info: info,
	}
}

func Must(err error) {
	if err != nil {
		fmt.Println(err)
		panic(err)
	}
}

func Must2(_ interface{}, err error) {
	if err != nil {
		fmt.Println(err)
		panic(err)
	}
}
