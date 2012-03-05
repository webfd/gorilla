// Copyright 2012 The Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sessions

import (
	"code.google.com/p/gorilla/context"
	"code.google.com/p/gorilla/securecookie"
	"encoding/gob"
	"errors"
	"fmt"
	"net/http"
)

// Default flashes key.
const flashesKey = "_flash"

// Options --------------------------------------------------------------------

// Options stores configuration for a session or session store.
//
// Fields are a subset of http.Cookie fields.
type Options struct {
	Path   string
	Domain string
	// MaxAge=0 means no 'Max-Age' attribute specified.
	// MaxAge<0 means delete cookie now, equivalently 'Max-Age: 0'.
	// MaxAge>0 means Max-Age attribute present and given in seconds.
	MaxAge   int
	Secure   bool
	HttpOnly bool
}

// Session --------------------------------------------------------------------

// NewSession is called by session stores to create a new session instance.
func NewSession(store Store, name string) *Session {
	return &Session{
		Values: make(map[interface{}]interface{}),
		store:  store,
		name:   name,
	}
}

// Session stores the values and optional configuration for a session.
type Session struct {
	Values  map[interface{}]interface{}
	Options *Options
	IsNew   bool
	store   Store
	name    string
}

// Flashes returns a slice of flash messages from the session.
//
// A single variadic argument is accepted, and it is optional: it defines
// the flash key. If not defined "_flash" is used by default.
func (s *Session) Flashes(vars ...string) []interface{} {
	key := flashesKey
	if len(vars) > 0 {
		key = vars[0]
	}
	if flashes, ok := s.Values[key]; ok {
		// Drop the flashes and return it.
		delete(s.Values, key)
		return flashes.([]interface{})
	}
	// Return a new one.
	return make([]interface{}, 0)
}

// AddFlash adds a flash message to the session.
//
// A single variadic argument is accepted, and it is optional: it defines
// the flash key. If not defined "_flash" is used by default.
func (s *Session) AddFlash(value interface{}, vars ...string) {
	key := flashesKey
	if len(vars) > 0 {
		key = vars[0]
	}
	var flashes []interface{}
	if v, ok := s.Values[key]; ok {
		flashes = v.([]interface{})
	}
	s.Values[key] = append(flashes, value)
}

// Save is a convenience method to save this session. It is the same as calling
// store.Save(request, response, session)
func (s *Session) Save(r *http.Request, w http.ResponseWriter) error {
	return s.store.Save(r, w, s)
}

// Name returns the name used to register the session.
func (s *Session) Name() string {
	return s.name
}

// Store returns the session store used to register the session.
func (s *Session) Store() Store {
	return s.store
}

// Registry -------------------------------------------------------------------

// sessionInfo stores a session tracked by the registry.
type sessionInfo struct {
	s *Session
	e error
}

// contextKey is the type used to store the registry in the context.
type contextKey int

// sessionsKey is the key used to store the registry in the context.
const sessionsKey contextKey = 0

// GetRegistry returns the registry instance for the current request.
func GetRegistry(r *http.Request) *Registry {
	s := context.DefaultContext.Get(r, sessionsKey)
	if s != nil {
		return s.(*Registry)
	}
	sessions := &Registry{
		request:  r,
		sessions: make(map[string]sessionInfo),
	}
	context.DefaultContext.Set(r, sessionsKey, sessions)
	return sessions
}

// Registry stores sessions used during a request.
type Registry struct {
	request  *http.Request
	sessions map[string]sessionInfo
}

// Get registers and returns a session for the given name and session store.
//
// It returns a new session if there are no sessions registered for the name.
func (s *Registry) Get(store Store, name string) (*Session, error) {
	if info, ok := s.sessions[name]; ok {
		info.s.store = store
		return info.s, info.e
	}
	session, err := store.New(s.request, name)
	session.store = store
	session.name = name
	s.sessions[name] = sessionInfo{s: session, e: err}
	return session, err
}

// Save saves all sessions registered for the current request.
func (s *Registry) Save(w http.ResponseWriter) error {
	var errMulti MultiError
	for name, info := range s.sessions {
		session := info.s
		if session.store == nil {
			errMulti = append(errMulti, fmt.Errorf(
				"sessions: missing store for session %q", name))
		} else if err := session.store.Save(s.request, w, session); err != nil {
			errMulti = append(errMulti, fmt.Errorf(
				"sessions: error saving session %q -- %v", name, err))
		}
	}
	if errMulti != nil {
		return errMulti
	}
	return nil
}

// Helpers --------------------------------------------------------------------

func init() {
	gob.Register([]interface{}{})
}

// Save saves all sessions used during the current request.
func Save(r *http.Request, w http.ResponseWriter) error {
	return GetRegistry(r).Save(w)
}

// EncodeCookie encodes a cookie value using a group of securecookie codecs.
//
// The codecs are tried in order. Multiple codecs are accepted to allow
// key rotation.
func EncodeCookie(name string, value interface{},
	codecs ...securecookie.Codec) (string, error) {
	for _, codec := range codecs {
		if encoded, err := codec.Encode(name, value); err == nil {
			return encoded, nil
		}
	}
	return "", errors.New("sessions: cookie could not be encoded")
}

// DecodeCookie decodes a cookie value using a group of securecookie codecs.
//
// The codecs are tried in order. Multiple codecs are accepted to allow
// key rotation.
func DecodeCookie(name string, value string, dst *map[interface{}]interface{},
	codecs ...securecookie.Codec) error {
	for _, codec := range codecs {
		if err := codec.Decode(name, value, dst); err == nil {
			return nil
		}
	}
	return errors.New("sessions: cookie could not be decoded")
}

// Error ----------------------------------------------------------------------

// MultiError stores multiple errors.
//
// Borrowed from the App Engine SDK.
type MultiError []error

func (m MultiError) Error() string {
	s, n := "", 0
	for _, e := range m {
		if e != nil {
			if n == 0 {
				s = e.Error()
			}
			n++
		}
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
