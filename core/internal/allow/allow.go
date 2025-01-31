package allow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/scanner"

	"gopkg.in/yaml.v3"

	"github.com/chirino/graphql/schema"
	"github.com/dosco/graphjin/core/internal/graph"
	"github.com/dosco/graphjin/internal/jsn"
	"github.com/spf13/afero"
)

const (
	expComment = iota + 1
	expVar
	expQuery
	expFrag
)

const (
	queryPath    = "/queries"
	fragmentPath = "/fragments"
)

type Item struct {
	Namespace string `yaml:",omitempty"`
	Name      string
	Comment   string `yaml:",omitempty"`
	key       string
	Query     string
	Vars      string   `yaml:",omitempty"`
	Metadata  Metadata `yaml:",inline,omitempty"`
	frags     []Frag
}

type Metadata struct {
	Order struct {
		Var    string   `yaml:"var,omitempty"`
		Values []string `yaml:"values,omitempty"`
	} `yaml:",omitempty"`
}

type Frag struct {
	Name  string
	Value string
}

type List struct {
	saveChan chan Item
	fs       afero.Fs
}

type Config struct {
	Log *log.Logger
}

func NewReadOnly(fs afero.Fs) (*List, error) {
	return &List{fs: fs}, nil
}

func New(conf Config, fs afero.Fs) (*List, error) {
	if fs == nil {
		return nil, fmt.Errorf("no filesystem defined for the allow list")
	}

	al := List{saveChan: make(chan Item), fs: fs}

	_ = fs.MkdirAll(queryPath, os.ModePerm)
	_ = fs.MkdirAll(fragmentPath, os.ModePerm)

	var err error

	go func() {
		for {
			v, ok := <-al.saveChan
			if !ok {
				break
			}
			err = al.save(v)
			if err != nil && conf.Log != nil {
				conf.Log.Println("WRN allow list save:", err)
			}
		}
	}()

	return &al, err
}

func (al *List) Set(vars []byte, query string, md Metadata, namespace string) error {
	if al.saveChan == nil {
		return errors.New("allow list is read-only")
	}

	if query == "" {
		return errors.New("empty query")
	}

	item, err := parseQuery(query)
	if err != nil {
		return err
	}

	item.Namespace = namespace
	item.Vars = string(vars)
	item.Metadata = md
	al.saveChan <- item
	return nil
}

func (al *List) Load() ([]Item, error) {
	var items []Item
	var files []fs.FileInfo
	var err error

	if ok, err := afero.DirExists(al.fs, queryPath); !ok {
		return items, nil
	} else if err != nil {
		return nil, fmt.Errorf("allow list: %w", err)
	}

	files, err = afero.ReadDir(al.fs, queryPath)
	if err != nil {
		return nil, fmt.Errorf("allow list: %w", err)
	}

	for _, f := range files {
		if f.IsDir() {
			continue
		}

		item, err := al.Get(filepath.Join(queryPath, f.Name()))
		if err == errUnknownFileType {
			continue
		}
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (al *List) GetByName(filePath string) (Item, error) {
	var item Item
	fpath := filepath.Join(queryPath, filePath)

	fn := (fpath + ".gql")
	if ok, err := afero.Exists(al.fs, fn); ok {
		return al.Get(fn)
	} else if err != nil {
		return item, err
	}

	fn = (fpath + ".graphql")
	if ok, err := afero.Exists(al.fs, fn); ok {
		return al.Get(fn)
	} else if err != nil {
		return item, err
	}

	fn = (fpath + ".yml")
	if ok, err := afero.Exists(al.fs, fn); ok {
		return al.Get(fn)
	} else if err != nil {
		return item, err
	}

	fn = (fpath + ".yaml")
	if ok, err := afero.Exists(al.fs, fn); ok {
		return al.Get(fn)
	} else if err != nil {
		return item, err
	}

	return item, nil
}

var errUnknownFileType = errors.New("unknown filetype")

func (al *List) Get(filePath string) (Item, error) {
	var item Item

	switch filepath.Ext(filePath) {
	case ".gql", ".graphql":
		return itemFromGQL(al.fs, filePath)
	case ".yml", ".yaml":
		return itemFromYaml(al.fs, filePath)
	default:
		return item, errUnknownFileType
	}
}

func itemFromYaml(fs afero.Fs, filePath string) (Item, error) {
	var item Item

	b, err := afero.ReadFile(fs, filePath)
	if err != nil {
		return item, err
	}

	if err := yaml.Unmarshal(b, &item); err != nil {
		return item, err
	}
	return item, nil
}

func itemFromGQL(fs afero.Fs, filePath string) (Item, error) {
	var item Item

	fn := filepath.Base(filePath)
	fn = strings.TrimSuffix(fn, filepath.Ext(fn))
	queryNS, queryName := splitName(fn)

	if queryName == "" {
		return item, fmt.Errorf("invalid filename: %s", filePath)
	}

	query, err := parseGQLFile(fs, filePath)
	if err != nil {
		return item, err
	}

	// h, err := graph.FastParse(query)
	// if err != nil {
	// 	return item, err
	// }

	item.Namespace = queryNS
	item.Name = queryName
	item.Query = query
	item.key = strings.ToLower(item.Name)

	return item, nil
}

func parseQuery(b string) (Item, error) {
	var s scanner.Scanner
	s.Init(strings.NewReader(b))
	s.Mode ^= scanner.SkipComments

	var op, sp scanner.Position
	var item Item
	var err error

	st := expComment

	for tok := s.Scan(); tok != scanner.EOF; tok = s.Scan() {
		txt := s.TokenText()

		switch {
		case strings.HasPrefix(txt, "/*"):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = s.Pos()

		case strings.HasPrefix(txt, "variables"):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = s.Pos()
			st = expVar

		case isGraphQL(txt):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = op
			st = expQuery

		case strings.HasPrefix(txt, "fragment"):
			v := b[sp.Offset:s.Pos().Offset]
			item, err = setValue(st, v, item)
			sp = op
			st = expFrag
		}

		if err != nil {
			return item, err
		}

		op = s.Pos()
	}

	if st == expQuery || st == expFrag {
		v := b[sp.Offset:s.Pos().Offset]
		item, err = setValue(st, v, item)
	}

	if err != nil {
		return item, err
	}

	item.key = strings.ToLower(item.Name)
	return item, nil
}

func setValue(st int, v string, item Item) (Item, error) {
	val := func() string {
		return strings.TrimSpace(v[:strings.LastIndexByte(v, '}')+1])
	}
	switch st {
	case expComment:
		item.Comment = val()

	case expVar:
		item.Vars = val()

	case expQuery:
		item.Query = val()

	case expFrag:
		f := Frag{Value: val()}
		f.Name = fragmentName(f.Value)
		item.frags = append(item.frags, f)
	}

	return item, nil
}

func (al *List) save(item Item) error {
	var buf bytes.Buffer

	qd := &schema.QueryDocument{}
	if err := qd.Parse(item.Query); err != nil {
		return err
	}

	qd.WriteTo(&buf)
	query := buf.String()
	buf.Reset()

	h, err := graph.FastParse(query)
	if err != nil {
		return err
	}

	if h.Name == "" {
		return errors.New("no query name defined. only named queries are saved to the allow list")
	}

	item.Name = h.Name
	item.key = strings.ToLower(item.Name)

	if err := al.saveItem(item, true); err != nil {
		return err
	}

	return nil
}

func (al *List) saveItem(item Item, ow bool) error {
	var err error

	if item.Vars != "" {
		var buf bytes.Buffer

		if err := jsn.Clear(&buf, []byte(item.Vars)); err != nil {
			return err
		}

		vj := json.RawMessage(buf.Bytes())
		if vj, err = json.MarshalIndent(vj, "", "  "); err != nil {
			return err
		}
		item.Vars = string(vj)
	}

	var b bytes.Buffer
	y := yaml.NewEncoder(&b)
	y.SetIndent(2)
	err = y.Encode(&item)
	if err != nil {
		return err
	}

	var fn string
	if item.Namespace != "" {
		fn = item.Namespace + "." + item.Name + ".yaml"
	} else {
		fn = item.Name + ".yaml"
	}

	if err := afero.WriteFile(
		al.fs,
		filepath.Join(queryPath, fn),
		b.Bytes(),
		0600); err != nil {
		return err
	}

	for _, fv := range item.frags {
		if item.Namespace != "" {
			fn = item.Namespace + "." + fv.Name
		} else {
			fn = fv.Name
		}
		err := afero.WriteFile(
			al.fs,
			filepath.Join(fragmentPath, fn),
			[]byte(fv.Value),
			0600)

		if err != nil {
			return err
		}
	}

	return nil
}

func (al *List) FragmentFetcher(namespace string) func(name string) (string, error) {
	return func(name string) (string, error) {
		var fn string
		if namespace != "" {
			fn = namespace + "." + name
		} else {
			fn = name
		}
		v, err := afero.ReadFile(
			al.fs,
			filepath.Join(fragmentPath, fn))

		return string(v), err
	}
}

func splitName(v string) (string, string) {
	i := strings.LastIndex(v, ".")
	if i == -1 {
		return "", v
	} else if i < len(v)-1 {
		return v[:i], v[(i + 1):]
	}
	return "", ""
}
