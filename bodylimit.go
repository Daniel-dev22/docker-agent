package main

// Request body limit — every body-carrying route.
//
// The API is unauthenticated in-network and runs on hosts down to a Raspberry Pi;
// a handler binding JSON reads the whole body into memory. So every request with
// a body is bounded before any handler, or the idempotency store, sees it.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxRequestBodyBytes caps any request body. The largest real one is a register
// carrying a stack's files: every registered project's bundle on all 10 agents,
// re-encoded as POST /v1/projects {name, files, deploy}, is at most 12,047 bytes
// (kd-nuc01 otbr, measured 2026-09-17). The control-center editor sends exactly
// such a body (a compose file plus .env), and a bulk request at the 100-target cap
// with 64-character IDs is ~7 KB. 1 MiB is 87x the largest real body.
const maxRequestBodyBytes = 1 << 20

// boundedBody reads a request body of at most maxRequestBodyBytes into memory and
// hands handlers a replayable copy. A declared Content-Length over the limit is
// refused without reading a byte; an undeclared (chunked) body is refused as soon
// as it passes the limit.
func boundedBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			c.Next()
			return
		}
		if c.Request.ContentLength > maxRequestBodyBytes {
			refuseTooLarge(c)
			return
		}
		if c.Request.Body == nil || c.Request.Body == http.NoBody {
			c.Next()
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxRequestBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				refuseTooLarge(c)
				return
			}
			refuse(c, http.StatusBadRequest, "invalid_body", "read request body: "+echo(err.Error()), nil)
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		c.Next()
	}
}

func refuseTooLarge(c *gin.Context) {
	refuse(c, http.StatusRequestEntityTooLarge, "request_too_large",
		fmt.Sprintf("request body exceeds %d bytes", maxRequestBodyBytes), nil)
}
