package scraper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// object — JSON-объект, сохраняющий порядок ключей.
//
// Livewire v2 подписывает serverMemo HMAC'ом от json_encode(memo), а PHP кодирует
// массив в порядке вставки ключей. Если пересобрать memo через map[string]any,
// порядок ключей изменится, checksum не сойдётся и сервер ответит ошибкой.
// Поэтому значения хранятся как есть (json.RawMessage), а ключи — в исходном порядке.
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func newObject() *object {
	return &object{vals: make(map[string]json.RawMessage)}
}

func (o *object) Get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// Set заменяет значение на месте или добавляет ключ в конец — так же, как
// присваивание свойства JS-объекту в клиенте Livewire.
func (o *object) Set(key string, val json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = val
}

func (o *object) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("expected JSON object, got %v", tok)
	}
	*o = *newObject()
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("expected object key, got %v", tok)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return fmt.Errorf("decode value of %q: %w", key, err)
		}
		o.Set(key, val)
	}
	if _, err := dec.Token(); err != nil { // закрывающая '}'
		return err
	}
	return nil
}

func (o *object) MarshalJSON() ([]byte, error) {
	if o == nil {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

var errNoKey = errors.New("key not found")

// decodeKey декодирует значение по ключу в dst.
func (o *object) decodeKey(key string, dst any) error {
	raw, ok := o.Get(key)
	if !ok {
		return fmt.Errorf("%w: %q", errNoKey, key)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode %q: %w", key, err)
	}
	return nil
}
