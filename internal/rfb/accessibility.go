package rfb

import "encoding/binary"

const accessibilityServerHeaderSize = 14

func (c *Client) receiveAccessibility() error {
	header, err := c.stream.bytes(accessibilityServerHeaderSize)
	if err != nil {
		return err
	}
	length := int(binary.LittleEndian.Uint16(header[12:]))
	c.logger.Debug("accessibility message", "feature", binary.LittleEndian.Uint32(header[0:]),
		"message", binary.LittleEndian.Uint32(header[4:]), "reply-to", binary.LittleEndian.Uint32(header[8:]), "bytes", length)
	return c.stream.discard(length)
}
