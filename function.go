package gophper

import "context"

// Function is a PHP function written in Go.
//
// Arguments arrive as Go values: nil, bool, int64, float64, string, []any
// for a list (keys 0 to n-1, in order), and map[string]any for any other
// array. An object arrives as its properties. The result goes back the
// same way, and slices, arrays and maps of other types work too. An error
// becomes a RuntimeException in PHP, with its message.
//
// ctx is the run's. The script waits while the function runs.
type Function func(ctx context.Context, args []any) (any, error)
