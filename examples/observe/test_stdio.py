import os
import threading
import time
import unittest
from unittest.mock import patch

import stdio


class StdioTest(unittest.TestCase):
    def test_partial_writes_and_would_block_preserve_the_payload(self) -> None:
        read_fd, write_fd = os.pipe()
        payload = bytes(range(256)) * 1024
        received = bytearray()

        def receive() -> None:
            while chunk := os.read(read_fd, 4096):
                received.extend(chunk)

        reader = threading.Thread(target=receive)
        reader.start()
        native_write = os.write
        calls = 0

        def partial_write(descriptor: int, value: memoryview) -> int:
            nonlocal calls
            calls += 1
            if calls == 1:
                raise BlockingIOError()
            return native_write(descriptor, value[:1023])

        try:
            with os.fdopen(write_fd, "wb", buffering=0) as pipe:
                with patch("stdio.os.write", side_effect=partial_write):
                    stdio.write(pipe, payload, time.monotonic() + 5)
            reader.join(timeout=5)
            self.assertFalse(reader.is_alive())
            self.assertEqual(received, payload)
            self.assertGreater(calls, 2)
        finally:
            os.close(read_fd)
            reader.join(timeout=1)

    def test_unread_pipe_obeys_the_absolute_deadline(self) -> None:
        read_fd, write_fd = os.pipe()
        try:
            with os.fdopen(write_fd, "wb", buffering=0) as pipe:
                started = time.monotonic()
                with self.assertRaisesRegex(TimeoutError, "^stdio_write_timeout$"):
                    stdio.write(pipe, b"x" * (4 << 20), started + 0.1)
                self.assertLess(time.monotonic() - started, 1)
        finally:
            os.close(read_fd)


if __name__ == "__main__":
    unittest.main()
