package fasthttp

import pool "github.com/newacorn/bytes-pool"

// SetPrefixValue sets cookie value.
func (c *Cookie) SetPrefixValue(prefix string, value string) {
	if len(prefix) != 0 {
		c.value = append(c.value[:0], prefix...)
	}
	c.value = append(c.value, value...)
}

// SetPrefixValueBytes sets cookie value.
func (c *Cookie) SetPrefixValueBytes(prefix []byte, value []byte) {
	if len(prefix) != 0 {
		c.value = append(c.value[:0], prefix...)
	}
	c.value = append(c.value, value...)
}

func (h *ResponseHeader) RecycleItems() {
	for i := 0; i < len(h.recycleItems); i++ {
		h.recycleItems[i].RecycleToPool00()
	}
}

func (h *ResponseHeader) AddRecycleItems(items ...pool.Recycler) {
	h.recycleItems = append(h.recycleItems, items...)
}

func (h *ResponseHeader) ExtractRecycleItems() (rs []pool.Recycler) {
	rs = h.recycleItems
	h.recycleItems = h.recycleItems[:0]
	return
}

// SetCookieString sets 'key: value' cookies.
func (h *ResponseHeader) SetCookieString(key, value string) {
	h.cookies = setArg(h.cookies, key, value, argsHasValue)
}

// SetCookieBytesK sets 'key: value' cookies.
func (h *ResponseHeader) SetCookieBytesK(key []byte, value string) {
	h.SetCookieString(b2s(key), value)
}

// SetCookieBytesKV sets 'key: value' cookies.
func (h *ResponseHeader) SetCookieBytesKV(key, value []byte) {
	h.SetCookieString(b2s(key), b2s(value))
}
