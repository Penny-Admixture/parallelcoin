package mod

import (
	"git.parallelcoin.io/dev/pod/pkg/rpc/json"
)

type DuoGuiTemplates struct {
	App  map[string][]byte            `json:"app"`
	Data map[string]map[string][]byte `json:"data"`
}

type Blocks struct {
	// Per         int                          `json:"per"`
	// Page        int                          `json:"page"`
	CurrentPage int                          `json:"currentpage"`
	PageCount   int                          `json:"pagecount"`
	Blocks      []json.GetBlockVerboseResult `json:"blocks"`
}

type DbAddress string

type Address struct {
	Index   int     `json:"num"`
	Label   string  `json:"label"`
	Account string  `json:"account"`
	Address string  `json:"address"`
	Amount  float64 `json:"amount"`
}
type Send struct {
	// Phrase string  `json:"phrase"`
	// Addr   string  `json:"addr"`
	// Amount float64 `json:"amount"`
	//exit
}

type AddBook struct {
	Address string `json:"address"`
	Label   string `json:"label"`
}

type DuoGuiItem struct {
	Enabled  bool        `json:"enabled"`
	Name     string      `json:"name"`
	Slug     string      `json:"slug"`
	Version  string      `json:"ver"`
	CompType string      `json:"comptype"`
	SubType  string      `json:"subtype"`
	Data     interface{} `json:"data"`
}
type DuoGuiItems struct {
	Slug  string                `json:"slug"`
	Items map[string]DuoGuiItem `json:"items"`
}

type DuoVUEcomps []DuoVUEcomp

//  Vue App Model
type DuoVUEcomp struct {
	IsApp       bool   `json:"isapp"`
	Name        string `json:"name"`
	ID          string `json:"id"`
	Version     string `json:"ver"`
	Description string `json:"desc"`
	State       string `json:"state"`
	Image       string `json:"img"`
	URL         string `json:"url"`
	CompType    string `json:"comtype"`
	SubType     string `json:"subtype"`
	Js          string `json:"js"`
	Template    string `json:"template"`
	Css         string `json:"css"`
}

// Vue.use(EasyBar);
// Vue.use(moment);
// Vue.use('vue-grid-layout');
// Vue.use('vue-good-table');
// Vue.use(VueFormGenerator);
// Vue.use('vue-ace-editor');
