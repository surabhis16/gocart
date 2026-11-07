// This file only stops the Go tool from walking into node_modules: a directory
// with its own go.mod is a separate module, so ./... in web-service skips it.
module gocart/web-frontend

go 1.22
