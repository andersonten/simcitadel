package amqp

import (
	"context"
	"fmt"

	amqpdriver "github.com/rabbitmq/amqp091-go"
)

// Conn wraps an AMQP connection and provides channel management.
type Conn struct {
	conn *amqpdriver.Connection
}

// Dial connects to the AMQP broker at the given URL.
// Dial returns an error if the connection fails.
func Dial(ctx context.Context, url string) (*Conn, error) {
	cfg := amqpdriver.Config{}
	conn, err := amqpdriver.DialConfig(url, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial amqp %s: %w", url, err)
	}
	return &Conn{conn: conn}, nil
}

// Channel opens a new AMQP channel on the connection.
func (c *Conn) Channel() (*amqpdriver.Channel, error) {
	ch, err := c.conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("open amqp channel: %w", err)
	}
	return ch, nil
}

// IsClosed reports whether the AMQP connection is closed.
func (c *Conn) IsClosed() bool {
	return c.conn.IsClosed()
}

// Close closes the AMQP connection.
func (c *Conn) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("close amqp connection: %w", err)
	}
	return nil
}
