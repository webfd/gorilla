// Copyright 2011 Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mux

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"code.google.com/p/gorilla/context"
)

// All error descriptions.
const (
	// Parameter.
	errEmptyHost        = "Host() requires a non-zero string, got %q."
	errEmptyPath        = "Path() requires a non-zero string that starts with a slash, got %q."
	errEmptyPathPrefix  = "PathPrefix() requires a non-zero string that starts with a slash, got %q."
	errPairs            = "Parameters must be multiple of 2, got %v"
	// Template parsing.
	errUnbalancedBraces = "Unbalanced curly braces in route template: %q."
	errBadTemplatePart  = "Missing name or pattern in route template: %q."
	errVarName          = "Duplicated route variable name: %q."
	// URL building.
	errMissingRouteVar  = "Missing route variable: %q."
	errBadRouteVar      = "Route variable doesn't match: got %q, expected %q."
	errMissingHost      = "Route doesn't have a host."
	errMissingPath      = "Route doesn't have a path."
)

// ----------------------------------------------------------------------------
// Context
// ----------------------------------------------------------------------------

type contextKey int

const (
   varsKey contextKey = iota
   routeKey
)

// Vars returns the route variables for the current request, if any.
func Vars(r *http.Request) map[string]string {
	if rv := context.DefaultContext.Get(r, varsKey); rv != nil {
		return rv.(map[string]string)
	}
	return nil
}

// CurrentRoute returns the matched route for the current request, if any.
func CurrentRoute(r *http.Request) *Route {
	if rv := context.DefaultContext.Get(r, routeKey); rv != nil {
		return rv.(*Route)
	}
	return nil
}

func setVars(r *http.Request, val interface{}) {
	context.DefaultContext.Set(r, varsKey, val)
}

func setCurrentRoute(r *http.Request, val interface{}) {
	context.DefaultContext.Set(r, routeKey, val)
}

// ----------------------------------------------------------------------------
// Router
// ----------------------------------------------------------------------------

// Router registers routes to be matched and dispatches a handler.
//
// It implements the http.Handler interface, so it can be registered to serve
// requests:
//
//     var router = new(mux.Router)
//
//     func main() {
//         http.Handle("/", router)
//     }
//
// Or, for Google App Engine, register it in a init() function:
//
//     var router = new(mux.Router)
//
//     func init() {
//         http.Handle("/", router)
//     }
//
// This will send all incoming requests to the router.
type Router struct {
	// Routes to be matched, in order.
	Routes []*Route
	// Routes by name, for URL building.
	NamedRoutes map[string]*Route
	// Reference to the root router, where named routes are stored.
	rootRouter *Router
	// Configurable Handler to be used when no route matches.
	NotFoundHandler http.Handler
	// See Route.redirectSlash. This defines the default flag for new routes.
	redirectSlash bool
}

// root returns the root router, where named routes are stored.
func (r *Router) root() *Router {
	if r.rootRouter == nil {
		return r
	}
	return r.rootRouter
}

// Match matches registered routes against the request.
func (r *Router) Match(request *http.Request) (match *RouteMatch, ok bool) {
	for _, route := range r.Routes {
		if route.err != nil {
			continue
		}
		if match, ok = route.Match(request); ok {
			return
		}
	}
	return
}

// ServeHTTP dispatches the handler registered in the matched route.
//
// When there is a match, the route variables can be retrieved calling
// mux.Vars(request).
func (r *Router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// Clean path to canonical form and redirect.
	// (this comes from the http package)
	if p := cleanPath(request.URL.Path); p != request.URL.Path {
		writer.Header().Set("Location", p)
		writer.WriteHeader(http.StatusMovedPermanently)
		return
	}
	var handler http.Handler
	if match, ok := r.Match(request); ok {
		handler = match.Handler
	}
	if handler == nil {
		if r.NotFoundHandler == nil {
			r.NotFoundHandler = http.NotFoundHandler()
		}
		handler = r.NotFoundHandler
	}
	defer context.DefaultContext.Clear(request)
	handler.ServeHTTP(writer, request)
}

// AddRoute registers a route in the router.
func (r *Router) AddRoute(route *Route) *Router {
	if r.Routes == nil {
		r.Routes = make([]*Route, 0)
	}
	route.router = r
	r.Routes = append(r.Routes, route)
	return r
}

// RedirectSlash defines the default RedirectSlash behavior for new routes.
//
// See Route.RedirectSlash.
func (r *Router) RedirectSlash(value bool) *Router {
	r.redirectSlash = value
	return r
}

// Convenience route factories ------------------------------------------------

// NewRoute creates an empty route and registers it in the router.
func (r *Router) NewRoute() *Route {
	route := newRoute()
	route.redirectSlash = r.redirectSlash
	r.AddRoute(route)
	return route
}

// Handle registers a new route and sets a path and handler.
//
// See also: Route.Handle().
func (r *Router) Handle(path string, handler http.Handler) *Route {
	return r.NewRoute().Handle(path, handler)
}

// HandleFunc registers a new route and sets a path and handler function.
//
// See also: Route.HandleFunc().
func (r *Router) HandleFunc(path string, handler func(http.ResponseWriter,
	*http.Request)) *Route {
	return r.NewRoute().HandleFunc(path, handler)
}

// ----------------------------------------------------------------------------
// Route
// ----------------------------------------------------------------------------

// Route stores information to match a request.
type Route struct {
	// Reference to the router.
	router *Router
	// Request handler for this route.
	handler http.Handler
	// List of matchers.
	matchers []routeMatcher
	// Special case matcher: parsed template for host matching.
	hostTemplate *parsedTemplate
	// Special case matcher: parsed template for path matching.
	pathTemplate *parsedTemplate
	// Redirect access from paths not ending with slash to the slash'ed path
	// if the Route paths ends with a slash, and vice-versa.
	// If pattern is /path/, insert permanent redirect for /path.
	redirectSlash bool
	// The name associated with this route.
	name string
	// All errors encountered when building the route.
	err ErrMulti
}

// newRoute returns a new Route instance.
func newRoute() *Route {
	return &Route{
		matchers: make([]routeMatcher, 0),
	}
}

// Errors returns an ErrMulti with errors encountered when building the route.
func (r *Route) Errors() error {
	return r.err
}

// Clone clones a route.
func (r *Route) Clone() *Route {
	// Shallow copy is enough.
	c := *r
	return &c
}

// Match matches this route against the request.
//
// It sets variables from the matched route in the context, if any.
func (r *Route) Match(req *http.Request) (*RouteMatch, bool) {
	var hostMatches, pathMatches []string
	if r.hostTemplate != nil {
		hostMatches = r.hostTemplate.Regexp.FindStringSubmatch(req.URL.Host)
		if hostMatches == nil {
			return nil, false
		}
	}
	var redirectURL string
	if r.pathTemplate != nil {
		// TODO Match the path unescaped?
		/*
			if path, ok := url.URLUnescape(r.URL.Path); ok {
				// URLUnescape converts '+' into ' ' (space). Revert this.
				path = strings.Replace(path, " ", "+", -1)
			} else {
				path = r.URL.Path
			}
		*/
		pathMatches = r.pathTemplate.Regexp.FindStringSubmatch(req.URL.Path)
		if pathMatches == nil {
			return nil, false
		} else if r.redirectSlash {
			// Check if we should redirect.
			p1 := strings.HasSuffix(req.URL.Path, "/")
			p2 := strings.HasSuffix(r.pathTemplate.Template, "/")
			if p1 != p2 {
				ru, _ := url.Parse(req.URL.String())
				if p1 {
					ru.Path = ru.Path[:len(ru.Path)-1]
				} else {
					ru.Path += "/"
				}
				redirectURL = ru.String()
			}
		}
	}
	var match *RouteMatch
	if r.matchers != nil {
		for _, matcher := range r.matchers {
			if rv, ok := (matcher).Match(req); !ok {
				return nil, false
			} else if rv != nil {
				match = rv
				break
			}
		}
	}
	// We have a match.
	vars := make(map[string]string)
	if hostMatches != nil {
		for k, v := range r.hostTemplate.VarsN {
			vars[v] = hostMatches[k+1]
		}
	}
	if pathMatches != nil {
		for k, v := range r.pathTemplate.VarsN {
			vars[v] = pathMatches[k+1]
		}
	}
	if match == nil {
		match = &RouteMatch{Route: r, Handler: r.handler}
	}
	if redirectURL != "" {
		match.Handler = http.RedirectHandler(redirectURL, 301)
	}
	setVars(req, vars)
	setCurrentRoute(req, match.Route)
	return match, true
}

// Subrouting -----------------------------------------------------------------

// NewRouter creates a new router and adds it as a matcher for this route.
//
// This is used for subrouting: it will test the inner routes if other
// matchers matched. For example:
//
//     r := new(mux.Router)
//     subrouter := r.NewRoute().Host("www.domain.com").NewRouter()
//     subrouter.HandleFunc("/products/", ProductsHandler)
//     subrouter.HandleFunc("/products/{key}", ProductHandler)
//     subrouter.HandleFunc("/articles/{category}/{id:[0-9]+}"),
//                          ArticleHandler)
//
// In this example, the routes registered in the subrouter will only be tested
// if the host matches.
func (r *Route) NewRouter() *Router {
	router := &Router{
		Routes:     make([]*Route, 0),
		rootRouter: r.router.root(),
	}
	r.addMatcher(router)
	return router
}

// URL building ---------------------------------------------------------------

// URL builds a URL for the route. It returns nil in case of errors.
func (r *Route) URL(pairs ...string) *url.URL {
	u, _ := r.URLDebug(pairs...)
	return u
}

// URLHost builds a URL host for the route. It returns nil in case of errors.
func (r *Route) URLHost(pairs ...string) *url.URL {
	u, _ := r.URLHostDebug(pairs...)
	return u
}

// URLPath builds a URL path for the route. It returns nil in case of errors.
func (r *Route) URLPath(pairs ...string) *url.URL {
	u, _ := r.URLPathDebug(pairs...)
	return u
}

// URLDebug builds a URL for the route.
//
// It accepts a sequence of key/value pairs for the route variables. For
// example, given this route:
//
//     r := new(mux.Router)
//     r.HandleFunc("/articles/{category}/{id:[0-9]+}", ArticleHandler).
//       Name("article")
//
// ...a URL for it can be built using:
//
//     url := r.NamedRoutes["article"].URL("category", "technology",
//                                         "id", "42")
//
// ...which will return an url.URL with the following path:
//
//     "/articles/technology/42"
//
// This also works for host variables:
//
//     r := new(mux.Router)
//     r.NewRoute().Host("{subdomain}.domain.com").
//                  HandleFunc("/articles/{category}/{id:[0-9]+}", ArticleHandler).
//                  Name("article")
//
//     // url.String() will be "http://news.domain.com/articles/technology/42"
//     url := r.NamedRoutes["article"].URL("subdomain", "news",
//                                         "category", "technology",
//                                         "id", "42")
//
// All variable names defined in the route are required, and their values must
// conform to the corresponding patterns, if any.
//
// In case of bad arguments it will return nil.
func (r *Route) URLDebug(pairs ...string) (*url.URL, error) {
	var err error
	var values map[string]string
	var scheme, host, path string
	if values, err = stringMapFromPairs(pairs...); err != nil {
		return nil, fmt.Errorf("Route.URL: %v", err.Error())
	}
	if r.hostTemplate != nil {
		// Set a default scheme.
		scheme = "http"
		if host, err = reverseRoute(r.hostTemplate, values); err != nil {
			return nil, fmt.Errorf("Route.URL: %v", err.Error())
		}
	}
	if r.pathTemplate != nil {
		if path, err = reverseRoute(r.pathTemplate, values); err != nil {
			return nil, fmt.Errorf("Route.URL: %v", err.Error())
		}
	}
	return &url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   path,
	}, nil
}

// URLHostDebug builds the host part of the URL for a route.
//
// The route must have a host defined.
//
// In case of bad arguments or missing host it will return nil.
func (r *Route) URLHostDebug(pairs ...string) (*url.URL, error) {
	if r.hostTemplate == nil {
		return nil, errors.New(errMissingHost)
	}
	var err error
	var values map[string]string
	var host string
	if values, err = stringMapFromPairs(pairs...); err != nil {
		return nil, err
	}
	if host, err = reverseRoute(r.hostTemplate, values); err != nil {
		return nil, err
	}
	return &url.URL{
		Scheme: "http",
		Host:   host,
	}, nil
}

// URLPathDebug builds the path part of the URL for a route.
//
// The route must have a path defined.
//
// In case of bad arguments or missing path it will return nil.
func (r *Route) URLPathDebug(pairs ...string) (*url.URL, error) {
	if r.pathTemplate == nil {
		return nil, errors.New(errMissingPath)
	}
	var err error
	var path string
	var values map[string]string
	if values, err = stringMapFromPairs(pairs...); err != nil {
		return nil, err
	}
	if path, err = reverseRoute(r.pathTemplate, values); err != nil {
		return nil, err
	}
	return &url.URL{
		Path: path,
	}, nil
}

// reverseRoute builds a URL part based on the route's parsed template.
func reverseRoute(tpl *parsedTemplate, values map[string]string) (rv string, err error) {
	var value string
	var ok bool
	urlValues := make([]interface{}, len(tpl.VarsN))
	for k, v := range tpl.VarsN {
		if value, ok = values[v]; !ok {
			err = fmt.Errorf(errMissingRouteVar, v)
			return
		}
		urlValues[k] = value
	}
	rv = fmt.Sprintf(tpl.Reverse, urlValues...)
	if !tpl.Regexp.MatchString(rv) {
		// The URL is checked against the full regexp, instead of checking
		// individual variables. This is faster but to provide a good error
		// message, we check individual regexps if the URL doesn't match.
		for k, v := range tpl.VarsN {
			if !tpl.VarsR[k].MatchString(values[v]) {
				err = fmt.Errorf(errBadRouteVar, values[v],
					tpl.VarsR[k].String())
				return
			}
		}
	}
	return
}

// Route predicates -----------------------------------------------------------

// Handler sets a handler for the route.
func (r *Route) Handler(handler http.Handler) *Route {
	r.handler = handler
	return r
}

// HandlerFunc sets a handler function for the route.
func (r *Route) HandlerFunc(handler func(http.ResponseWriter,
	*http.Request)) *Route {
	return r.Handler(http.HandlerFunc(handler))
}

// Handle sets a path and handler for the route.
func (r *Route) Handle(path string, handler http.Handler) *Route {
	return r.Path(path).Handler(handler)
}

// HandleFunc sets a path and handler function for the route.
func (r *Route) HandleFunc(path string, handler func(http.ResponseWriter,
	*http.Request)) *Route {
	return r.Path(path).Handler(http.HandlerFunc(handler))
}

// Name sets the route name, used to build URLs.
//
// A name must be unique for a router. If the name was registered already
// it will be overwritten.
func (r *Route) Name(name string) *Route {
	router := r.router.root()
	if router.NamedRoutes == nil {
		router.NamedRoutes = make(map[string]*Route)
	}
	r.name = name
	router.NamedRoutes[name] = r
	return r
}

// GetName returns the name associated with a route, if any.
func (r *Route) GetName() string {
	return r.name
}

// RedirectSlash defines the redirectSlash behavior for this route.
//
// When true, if the route path is /path/, accessing /path will redirect to
// /path/, and vice versa.
func (r *Route) RedirectSlash(value bool) *Route {
	r.redirectSlash = value
	return r
}

// Route matchers -------------------------------------------------------------

// addMatcher adds a matcher to the array of route matchers.
func (r *Route) addMatcher(m routeMatcher) *Route {
	r.matchers = append(r.matchers, m)
	return r
}

// Headers adds a matcher to match the request against header values.
//
// It accepts a sequence of key/value pairs to be matched. For example:
//
//     r := new(mux.Router)
//     r.NewRoute().Headers("Content-Type", "application/json",
//                          "X-Requested-With", "XMLHttpRequest")
//
// The above route will only match if both request header values match.
//
// It the value is an empty string, it will match any value if the key is set.
func (r *Route) Headers(pairs ...string) *Route {
	if len(pairs) == 0 {
		return r
	}
	headers, err := stringMapFromPairs(pairs...)
	if err != nil {
		r.err = append(r.err, err)
		return r
	}
	return r.addMatcher(&headerMatcher{headers: headers})
}

// Host adds a matcher to match the request against the URL host.
//
// It accepts a template with zero or more URL variables enclosed by {}.
// Variables can define an optional regexp pattern to me matched:
//
// - {name} matches anything until the next dot.
//
// - {name:pattern} matches the given regexp pattern.
//
// For example:
//
//     r := new(mux.Router)
//     r.NewRoute().Host("www.domain.com")
//     r.NewRoute().Host("{subdomain}.domain.com")
//     r.NewRoute().Host("{subdomain:[a-z]+}.domain.com")
//
// Variable names must be unique in a given route. They can be retrieved
// calling mux.Vars(request).
func (r *Route) Host(template string) *Route {
	if template == "" {
		r.err = append(r.err, fmt.Errorf(errEmptyHost, template))
		return r
	}
	tpl := &parsedTemplate{Template: template}
	vars := variableNames(r.pathTemplate)
	if err := parseTemplate(tpl, "[^.]+", false, false, vars); err != nil {
		r.err = append(r.err, err)
		return r
	}
	r.hostTemplate = tpl
	return r
}

// Matcher adds a matcher to match the request using a custom function.
func (r *Route) Matcher(matcherFunc MatcherFunc) *Route {
	return r.addMatcher(matcherFunc)
}

// Methods adds a matcher to match the request against HTTP methods.
//
// It accepts a sequence of one or more methods to be matched, e.g.:
// "GET", "POST", "PUT".
func (r *Route) Methods(methods ...string) *Route {
	if len(methods) == 0 {
		return r
	}
	for k, v := range methods {
		methods[k] = strings.ToUpper(v)
	}
	return r.addMatcher(&methodMatcher{methods: methods})
}

// Path adds a matcher to match the request against the URL path.
//
// It accepts a template with zero or more URL variables enclosed by {}.
// Variables can define an optional regexp pattern to me matched:
//
// - {name} matches anything until the next slash.
//
// - {name:pattern} matches the given regexp pattern.
//
// For example:
//
//     r := new(mux.Router)
//     r.NewRoute().Path("/products/").Handler(ProductsHandler)
//     r.NewRoute().Path("/products/{key}").Handler(ProductsHandler)
//     r.NewRoute().Path("/articles/{category}/{id:[0-9]+}").
//                  Handler(ArticleHandler)
//
// Variable names must be unique in a given route. They can be retrieved
// calling mux.Vars(request).
func (r *Route) Path(template string) *Route {
	if template == "" || template[0] != '/' {
		r.err = append(r.err, fmt.Errorf(errEmptyPath, template))
		return r
	}
	tpl := &parsedTemplate{Template: template}
	vars := variableNames(r.hostTemplate)
	if err := parseTemplate(tpl, "[^/]+", false, r.redirectSlash, vars); err != nil {
		r.err = append(r.err, err)
		return r
	}
	r.pathTemplate = tpl
	return r
}

// PathPrefix adds a matcher to match the request against a URL path prefix.
func (r *Route) PathPrefix(template string) *Route {
	if template == "" || template[0] != '/' {
		r.err = append(r.err, fmt.Errorf(errEmptyPathPrefix, template))
		return r
	}
	tpl := &parsedTemplate{Template: template}
	vars := variableNames(r.hostTemplate)
	if err := parseTemplate(tpl, "[^/]+", true, false, vars); err != nil {
		r.err = append(r.err, err)
		return r
	}
	r.pathTemplate = tpl
	return r
}

// Queries adds a matcher to match the request against URL query values.
//
// It accepts a sequence of key/value pairs to be matched. For example:
//
//     r := new(mux.Router)
//     r.NewRoute().Queries("foo", "bar",
//                          "baz", "ding")
//
// The above route will only match if the URL contains the defined queries
// values, e.g.: ?foo=bar&baz=ding.
//
// It the value is an empty string, it will match any value if the key is set.
func (r *Route) Queries(pairs ...string) *Route {
	if len(pairs) == 0 {
		return r
	}
	queries, err := stringMapFromPairs(pairs...)
	if err != nil {
		r.err = append(r.err, err)
		return r
	}
	return r.addMatcher(&queryMatcher{queries: queries})
}

// Schemes adds a matcher to match the request against URL schemes.
//
// It accepts a sequence of one or more schemes to be matched, e.g.:
// "http", "https".
func (r *Route) Schemes(schemes ...string) *Route {
	if len(schemes) == 0 {
		return r
	}
	for k, v := range schemes {
		schemes[k] = strings.ToLower(v)
	}
	return r.addMatcher(&schemeMatcher{schemes: schemes})
}

// ----------------------------------------------------------------------------
// Matchers
// ----------------------------------------------------------------------------

// routeMatch is the returned result when a route matches.
type RouteMatch struct {
	Route   *Route
	Handler http.Handler
}

// MatcherFunc is the type used by custom matchers.
type MatcherFunc func(*http.Request) bool

// Match matches the request using a custom matcher function.
func (m MatcherFunc) Match(request *http.Request) (*RouteMatch, bool) {
	return nil, m(request)
}

// routeMatcher is the interface used by the router, route and route matchers.
//
// Only Router and Route actually return a route; it indicates a final match.
// Route matchers return nil and the result from the individual match.
type routeMatcher interface {
	Match(*http.Request) (*RouteMatch, bool)
}

// headerMatcher matches the request against header values.
type headerMatcher struct {
	headers map[string]string
}

func (m *headerMatcher) Match(request *http.Request) (*RouteMatch, bool) {
	return nil, matchMap(m.headers, request.Header, true)
}

// methodMatcher matches the request against HTTP methods.
type methodMatcher struct {
	methods []string
}

func (m *methodMatcher) Match(request *http.Request) (*RouteMatch, bool) {
	return nil, matchInArray(m.methods, request.Method)
}

// queryMatcher matches the request against URL queries.
type queryMatcher struct {
	queries map[string]string
}

func (m *queryMatcher) Match(request *http.Request) (*RouteMatch, bool) {
	return nil, matchMap(m.queries, request.URL.Query(), false)
}

// schemeMatcher matches the request against URL schemes.
type schemeMatcher struct {
	schemes []string
}

func (m *schemeMatcher) Match(request *http.Request) (*RouteMatch, bool) {
	return nil, matchInArray(m.schemes, request.URL.Scheme)
}

// ----------------------------------------------------------------------------
// Template parsing
// ----------------------------------------------------------------------------

// parsedTemplate stores a regexp and variables info for a route matcher.
type parsedTemplate struct {
	// The unmodified template.
	Template string
	// Expanded regexp.
	Regexp *regexp.Regexp
	// Reverse template.
	Reverse string
	// Variable names.
	VarsN []string
	// Variable regexps (validators).
	VarsR []*regexp.Regexp
}

// parseTemplate parses a route template, expanding variables into regexps.
//
// It will extract named variables, assemble a regexp to be matched, create
// a "reverse" template to build URLs and compile regexps to validate variable
// values used in URL building.
//
// Previously we accepted only Python-like identifiers for variable
// names ([a-zA-Z_][a-zA-Z0-9_]*), but currently the only restriction is that
// name and pattern can't be empty, and names can't contain a colon.
func parseTemplate(tpl *parsedTemplate, defaultPattern string, prefix bool,
	redirectSlash bool, names *[]string) error {
	// Set a flag for redirectSlash.
	template := tpl.Template
	endSlash := false
	if redirectSlash && strings.HasSuffix(template, "/") {
		template = template[:len(template)-1]
		endSlash = true
	}

	idxs, err := getBraceIndices(template)
	if err != nil {
		return err
	}

	var raw, name, patt string
	var end int
	var parts []string
	pattern := bytes.NewBufferString("^")
	reverse := bytes.NewBufferString("")
	size := len(idxs)
	tpl.VarsN = make([]string, size/2)
	tpl.VarsR = make([]*regexp.Regexp, size/2)
	for i := 0; i < size; i += 2 {
		// 1. Set all values we are interested in.
		raw = template[end:idxs[i]]
		end = idxs[i+1]
		parts = strings.SplitN(template[idxs[i]+1:end-1], ":", 2)
		name = parts[0]
		if len(parts) == 1 {
			patt = defaultPattern
		} else {
			patt = parts[1]
		}
		// Name or pattern can't be empty.
		if name == "" || patt == "" {
			return fmt.Errorf(errBadTemplatePart, template[idxs[i]:end])
		}
		// Name must be unique for the route.
		if names != nil {
			if matchInArray(*names, name) {
				return fmt.Errorf(errVarName, name)
			}
			*names = append(*names, name)
		}
		// 2. Build the regexp pattern.
		fmt.Fprintf(pattern, "%s(%s)", regexp.QuoteMeta(raw), patt)
		// 3. Build the reverse template.
		fmt.Fprintf(reverse, "%s%%s", raw)
		// 4. Append variable name and compiled pattern.
		tpl.VarsN[i/2] = name
		if reg, err := regexp.Compile(fmt.Sprintf("^%s$", patt)); err != nil {
			return err
		} else {
			tpl.VarsR[i/2] = reg
		}
	}
	// 5. Add the remaining.
	raw = template[end:]
	pattern.WriteString(regexp.QuoteMeta(raw))
	if redirectSlash {
		pattern.WriteString("[/]?")
	}
	if !prefix {
		pattern.WriteString("$")
	}
	reverse.WriteString(raw)
	if endSlash {
		reverse.WriteString("/")
	}
	// Done!
	reg, err := regexp.Compile(pattern.String())
	if err != nil {
		return err
	}
	tpl.Regexp = reg
	tpl.Reverse = reverse.String()
	return nil
}

// getBraceIndices returns index bounds for route template variables.
//
// It will return an error if there are unbalanced braces.
func getBraceIndices(s string) ([]int, error) {
	var level, idx int
	idxs := make([]int, 0)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			if level++; level == 1 {
				idx = i
			}
		case '}':
			if level--; level == 0 {
				idxs = append(idxs, idx, i+1)
			} else if level < 0 {
				return nil, fmt.Errorf(errUnbalancedBraces, s)
			}
		}
	}
	if level != 0 {
		return nil, fmt.Errorf(errUnbalancedBraces, s)
	}
	return idxs, nil
}

// ----------------------------------------------------------------------------
// ErrMulti
// ----------------------------------------------------------------------------

// ErrMulti stores multiple errors.
type ErrMulti []error

// String returns a string representation of the error.
func (m ErrMulti) Error() string {
	s, n := "", 0
	for _, e := range m {
		if e == nil {
			continue
		}
		if n == 0 {
			s = e.Error()
		}
		n++
	}
	switch n {
	case 0:
		return "(0 errors)"
	case 1:
		return s
	case 2:
		return s + " (and 1 other error)"
	}
	return fmt.Sprintf("%s (and %d other errors)", s, n-1)
}

// ----------------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------------

// cleanPath returns the canonical path for p, eliminating . and .. elements.
//
// Extracted from the http package.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root;
	// put the trailing slash back if necessary.
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np
}

// stringMapFromPairs converts variadic string parameters to a string map.
func stringMapFromPairs(pairs ...string) (map[string]string, error) {
	length := len(pairs)
	if length%2 != 0 {
		return nil, fmt.Errorf(errPairs, pairs)
	}
	m := make(map[string]string, length/2)
	for i := 0; i < length; i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return m, nil
}

// variableNames returns a copy of variable names for route templates.
func variableNames(tpl *parsedTemplate) *[]string {
	var names []string
	if tpl != nil {
		names = tpl.VarsN
	}
	return &names
}

// matchInArray returns true if the given string value is in the array.
func matchInArray(arr []string, value string) bool {
	for _, v := range arr {
		if v == value {
			return true
		}
	}
	return false
}

// matchMap returns true if the given key/value pairs exist in a given map.
func matchMap(toCheck map[string]string, toMatch map[string][]string,
	canonicalKey bool) bool {
	for k, v := range toCheck {
		// Check if key exists.
		if canonicalKey {
			k = http.CanonicalHeaderKey(k)
		}
		if values := toMatch[k]; values == nil {
			return false
		} else if v != "" {
			// If value was defined as an empty string we only check that the
			// key exists. Otherwise we also check if the value exists.
			valueExists := false
			for _, value := range values {
				if v == value {
					valueExists = true
					break
				}
			}
			if !valueExists {
				return false
			}
		}
	}
	return true
}