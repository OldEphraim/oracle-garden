package polymarket

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Market is a single Polymarket market resolved via Gamma /markets/{slug}.
//
// Polymarket's Gamma API stringifies several numeric and array fields on the
// wire. The exported struct uses native Go types — UnmarshalJSON below handles
// the wire-format coercion so callers never see strings where numbers belong.
type Market struct {
	ID            string    `json:"id"`
	Slug          string    `json:"slug"`
	Question      string    `json:"question"`
	ConditionID   string    `json:"conditionId"`
	Description   string    `json:"description"`
	Outcomes      []string  `json:"outcomes"`       // wire: JSON-encoded string of array of strings.
	OutcomePrices []float64 `json:"outcomePrices"`  // wire: JSON-encoded string of array of (numeric) strings.
	ClobTokenIDs  []string  `json:"clobTokenIds"`   // wire: JSON-encoded string of array of strings.
	Liquidity     float64   `json:"liquidity"`      // wire: numeric string OR number.
	Volume24Hr    float64   `json:"volume24hr"`     // wire: number (or numeric string in older payloads).
	EndDate       time.Time `json:"endDate"`        // wire: RFC3339 string.
	Active        bool      `json:"active"`
	Closed        bool      `json:"closed"`
	Archived      bool      `json:"archived"`
	Tags          []string  `json:"tags"`
}

// marketWire mirrors the on-the-wire shape: the fields Polymarket stringifies
// stay as flex* types so we can decode either form, then we copy into Market.
type marketWire struct {
	ID            string          `json:"id"`
	Slug          string          `json:"slug"`
	Question      string          `json:"question"`
	ConditionID   string          `json:"conditionId"`
	Description   string          `json:"description"`
	Outcomes      flexStringArray `json:"outcomes"`
	OutcomePrices flexFloatArray  `json:"outcomePrices"`
	ClobTokenIDs  flexStringArray `json:"clobTokenIds"`
	Liquidity     flexFloat       `json:"liquidity"`
	Volume24Hr    flexFloat       `json:"volume24hr"`
	EndDate       string          `json:"endDate"`
	Active        bool            `json:"active"`
	Closed        bool            `json:"closed"`
	Archived      bool            `json:"archived"`
	Tags          []string        `json:"tags"`
}

func (m *Market) UnmarshalJSON(data []byte) error {
	var w marketWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.ID = w.ID
	m.Slug = w.Slug
	m.Question = w.Question
	m.ConditionID = w.ConditionID
	m.Description = w.Description
	m.Outcomes = []string(w.Outcomes)
	m.OutcomePrices = []float64(w.OutcomePrices)
	m.ClobTokenIDs = []string(w.ClobTokenIDs)
	m.Liquidity = float64(w.Liquidity)
	m.Volume24Hr = float64(w.Volume24Hr)
	m.Active = w.Active
	m.Closed = w.Closed
	m.Archived = w.Archived
	m.Tags = w.Tags
	if w.EndDate != "" {
		if t, err := time.Parse(time.RFC3339, w.EndDate); err == nil {
			m.EndDate = t
		}
	}
	return nil
}

// Orderbook is the response shape of CLOB /book?token_id=...
type Orderbook struct {
	Market    string           `json:"market"`     // conditionId hex
	AssetID   string           `json:"asset_id"`   // token_id
	Timestamp string           `json:"timestamp"`  // ms-since-epoch as string
	Hash      string           `json:"hash"`
	Bids      []OrderbookLevel `json:"bids"`
	Asks      []OrderbookLevel `json:"asks"`
}

// OrderbookLevel — CLOB returns both fields as numeric strings.
type OrderbookLevel struct {
	Price float64
	Size  float64
}

func (l *OrderbookLevel) UnmarshalJSON(data []byte) error {
	var w struct {
		Price flexFloat `json:"price"`
		Size  flexFloat `json:"size"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	l.Price = float64(w.Price)
	l.Size = float64(w.Size)
	return nil
}

// PriceSeries — CLOB /prices-history response. `t` is unix seconds (number),
// `p` is a number.
type PriceSeries struct {
	History []PricePoint `json:"history"`
}

// PricePoint — t is decoded into a time.Time for caller convenience.
type PricePoint struct {
	Time  time.Time
	Price float64
}

func (p *PricePoint) UnmarshalJSON(data []byte) error {
	var w struct {
		T int64     `json:"t"`
		P flexFloat `json:"p"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	p.Time = time.Unix(w.T, 0).UTC()
	p.Price = float64(w.P)
	return nil
}

// flexFloat accepts either a JSON number or a numeric string ("0.65"). Empty
// string and JSON null both decode to 0.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*f = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*f = 0
			return nil
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("flexFloat: parse %q: %w", s, err)
		}
		*f = flexFloat(v)
		return nil
	}
	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*f = flexFloat(v)
	return nil
}

// flexStringArray accepts:
//   - JSON array of strings:           ["Yes","No"]
//   - JSON-encoded string of an array: "[\"Yes\",\"No\"]"
//
// Polymarket's Gamma response uses the second form for outcomes/clobTokenIds.
type flexStringArray []string

func (a *flexStringArray) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*a = nil
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s == "" {
			*a = nil
			return nil
		}
		var out []string
		if err := json.Unmarshal([]byte(s), &out); err != nil {
			return fmt.Errorf("flexStringArray: parse inner %q: %w", s, err)
		}
		*a = out
		return nil
	}
	var out []string
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	*a = out
	return nil
}

// flexFloatArray accepts:
//   - JSON array of numbers:                [0.65, 0.35]
//   - JSON array of numeric strings:        ["0.65", "0.35"]
//   - JSON-encoded string of either form:   "[\"0.65\",\"0.35\"]"
//
// Polymarket's Gamma response uses the third form for outcomePrices.
type flexFloatArray []float64

func (a *flexFloatArray) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*a = nil
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s == "" {
			*a = nil
			return nil
		}
		return a.unmarshalArrayBytes([]byte(s))
	}
	return a.unmarshalArrayBytes(data)
}

func (a *flexFloatArray) unmarshalArrayBytes(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("flexFloatArray: parse array: %w", err)
	}
	out := make([]float64, len(raw))
	for i, item := range raw {
		var f flexFloat
		if err := f.UnmarshalJSON(item); err != nil {
			return fmt.Errorf("flexFloatArray[%d]: %w", i, err)
		}
		out[i] = float64(f)
	}
	*a = out
	return nil
}
