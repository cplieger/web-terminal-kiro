package main

import (
	"net/http"

	"github.com/cplieger/go/httpserver/v2"
	"github.com/cplieger/go/httpwire"
	"github.com/cplieger/toolbelt/v3/httpapi"
)

// codeSpecs are the codes this app answers that no library lists;
// httpservertest.CheckCodeSpecs holds them to the source.
var codeSpecs = map[httpserver.ErrorCode]httpserver.CodeSpec{
	"non_canonical_path": {Status: http.StatusBadRequest, Remediation: "Send the route path exactly as documented, without empty, dot or dot-dot segments.", NoEffect: true},
	"not_ready":          {Status: http.StatusServiceUnavailable, Remediation: "Wait for the Retry-After delay while the container finishes starting, then try again.", NoEffect: true},
	"upload_too_large":   {Status: http.StatusRequestEntityTooLarge, Remediation: "Paste a smaller image.", NoEffect: true},
	"upload_timeout":     {Status: http.StatusRequestTimeout, Remediation: "Paste the image again on a faster connection.", NoEffect: true},
	"invalid_filename":   {Status: http.StatusBadRequest, Remediation: "Name each file pasted-<UTC timestamp>.png, .jpg or .webp, as the page does when you paste.", NoEffect: true},
	"invalid_upload":     {Status: http.StatusBadRequest, Remediation: "Send a multipart body of 1 to 25 files in the files field.", NoEffect: true},
	"upload_failed":      {Status: http.StatusInternalServerError, Remediation: "Paste the image again; if it fails again, the server log names the cause."},
}

// catalog is every code the listener answers, the tools API's included, with
// its status and remediation. Its codes are program data, so a refusal to add
// one is a bug in this file and stops the process at start.
var catalog = func() *httpwire.Catalog {
	c := httpwire.NewCatalog()
	for code, spec := range codeSpecs {
		if err := c.Add(code, spec); err != nil {
			panic(err)
		}
	}
	if err := c.AddOwned(httpapi.CodeSpecs()); err != nil {
		panic(err)
	}
	for code, words := range map[httpserver.ErrorCode]string{
		"host_not_allowed": "Add the host name you browse to to ALLOWED_HOSTS, or browse to one it lists.",
		"rate_limited":     "Wait for the Retry-After delay, then open the tab again.",
		"loopback_only":    "Call this API from inside the container, on its loopback address.",
	} {
		if err := c.SetRemediation(code, words); err != nil {
			panic(err)
		}
	}
	return c
}()

// errorResponder writes every refusal the app's own layers and the tools API
// answer, at the catalog's status and with its remediation.
var errorResponder = catalog.Responder()
