// Copyright 2012 The Gorilla Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package schema

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

var invalidPath = errors.New("schema: invalid path")

type structCache struct {
	l sync.Mutex
	m map[string]*structInfo
}

func (c *structCache) parsePath(p string, t reflect.Type) ([]pathPart, error) {
	var struc *structInfo
	var field *fieldInfo
	var index64 int64
	var index int
	var err error
	raw := strings.Split(p, ".")
	res := make([]pathPart, 0)
	path := make([]int, 0)
	for i := 0; i < len(raw); i++ {
		if struc = c.get(t); struc == nil {
			return nil, invalidPath
		}
		if field = struc.get(raw[i]); field == nil {
			return nil, invalidPath
		}
		path = append(path, field.index)
		switch field.mainType.Kind() {
		case reflect.Struct:
			t = field.mainType
		case reflect.Slice:
			if field.elemType.Kind() == reflect.Struct {
				// i+1 must be the index, and i+2 must exist.
				i++
				if i+1 >= len(raw) {
					return nil, invalidPath
				}
				if index64, err = strconv.ParseInt(raw[i], 10, 0); err != nil {
					return nil, invalidPath
				}
				index = int(index64)
				res = append(res, pathPart{
					path: path,
					field: field,
					index: index,
				})
				path = make([]int, 0)
				t = field.elemType
			}
		}
	}
	res = append(res, pathPart{
		path: path,
		field: field,
		index: -1,
	})
	return res, nil
}

func (c *structCache) get(t reflect.Type) *structInfo {
	id := typeID(t)
	c.l.Lock()
	info := c.m[id]
	if info == nil {
		if info = c.create(t); info != nil {
			c.m[id] = info
		}
	}
	c.l.Unlock()
	return info
}

func (c *structCache) create(t reflect.Type) *structInfo {
	info := &structInfo{
		fields: make(map[string]*fieldInfo),
	}
	for i := 0; i < t.NumField(); i++ {
		// TODO: ignore unsupported field types.
		field := t.Field(i)
		alias := fieldAlias(field)
		// Ignore this field?
		if alias != "-" {
			info.fields[alias] = &fieldInfo{
				index:    i,
				alias:    alias,
				name:     field.Name,
				mainType: field.Type,
			}
			if field.Type.Kind() == reflect.Slice {
				info.fields[alias].elemType = field.Type.Elem()
			}
		}
	}
	return info
}

// ----------------------------------------------------------------------------

type structInfo struct {
	fields map[string]*fieldInfo
}

func (i *structInfo) get(alias string) *fieldInfo {
	return i.fields[alias]
}

// ----------------------------------------------------------------------------

type fieldInfo struct {
	index    int
	alias    string
	name     string
	mainType reflect.Type
	elemType reflect.Type
}

// ----------------------------------------------------------------------------

type pathPart struct {
	path  []int
	field *fieldInfo
	index int
}

// ----------------------------------------------------------------------------

// typeID returns a string identifier for a type.
func typeID(t reflect.Type) string {
	// Borrowed from gob package.
	// We don't care about pointers.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// Default to printed representation for unnamed types.
	name := t.String()
	// But for named types, qualify with import path.
	if t.Name() != "" {
		if t.PkgPath() == "" {
			name = t.Name()
		} else {
			name = t.PkgPath() + "." + t.Name()
		}
	}
	return name
}

// fieldAlias
func fieldAlias(field reflect.StructField) string {
	var alias string
	if tag := field.Tag.Get("schema"); tag != "" {
		// For now tags only support the name but let's folow the
		// comma convention from encoding/json and others.
		if idx := strings.Index(tag, ","); idx == -1 {
			alias = tag
		} else {
			alias = tag[:idx]
		}
	}
	if alias == "" {
		alias = field.Name
	}
	return alias
}