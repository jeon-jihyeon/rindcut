import os
import selectors
import time
from typing import BinaryIO


def write(pipe: BinaryIO, payload: bytes, deadline: float) -> None:
    descriptor = pipe.fileno()
    os.set_blocking(descriptor, False)
    remaining_payload = memoryview(payload)
    with selectors.DefaultSelector() as selector:
        selector.register(descriptor, selectors.EVENT_WRITE)
        while remaining_payload:
            remaining = deadline - time.monotonic()
            if remaining <= 0 or not selector.select(remaining):
                raise TimeoutError("stdio_write_timeout")
            if time.monotonic() >= deadline:
                raise TimeoutError("stdio_write_timeout")
            try:
                written = os.write(descriptor, remaining_payload)
            except BlockingIOError:
                continue
            if written <= 0:
                raise BrokenPipeError("stdio_write_failed")
            remaining_payload = remaining_payload[written:]
