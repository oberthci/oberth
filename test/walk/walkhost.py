"""Sample host load independently of command and assertion timing."""
from dataclasses import dataclass
import threading


@dataclass(frozen=True)
class Runtime:
    clock: object
    ids: object
    load: object


class Load:
    def __init__(self, read):
        self.read, self.samples = read, []
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.watch, daemon=True)

    def sample(self):
        value = self.read()
        self.samples.append(value)
        return value

    def watch(self):
        while not self.stop.wait(1):
            self.sample()

    def __enter__(self):
        self.sample()
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.stop.set()
        self.thread.join()

    @property
    def busy(self):
        return any(sample[1] > 12 for sample in self.samples)
