package client

// Waiters reports how many AwaitTX calls are waiting for the TX window.
func (c *Client) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.txWaiters)
}
