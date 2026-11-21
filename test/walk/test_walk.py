"""Independent image-publication contract."""
import gzip
import importlib.util
import io
import json
from pathlib import Path
import tarfile
import unittest
from unittest.mock import patch
spec = importlib.util.spec_from_file_location("publisher", Path(__file__).parent / "fixture/publish.py")
publisher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publisher)


class Response:
    def __init__(self, body=b"", headers=None):
        self.body, self.headers = body, headers or {}

    def __enter__(self):
        return self

    def __exit__(self, *args):
        pass

    def read(self):
        return self.body


class PublisherTests(unittest.TestCase):
    def test_image_contains_the_built_binary_and_roundtrips_exact_manifest(self):
        uploads, manifest = [], []
        class Transport:
            def open(self, request, timeout):
                if request.get_method() == "POST":
                    return Response(headers={"Location": "/v2/fixture/blobs/uploads/id"})
                if "blobs/uploads/id?digest=" in request.full_url:
                    self_digest = publisher.digest(request.data)
                    if self_digest not in request.full_url.replace("%3A", ":"):
                        raise AssertionError("Uploaded blob differs from its digest")
                    uploads.append(request.data)
                elif request.get_method() == "PUT":
                    manifest.append(request.data)
                elif request.get_method() == "GET":
                    return Response(manifest[0])
                return Response()
        with patch.object(publisher.urllib.request, "build_opener", return_value=Transport()):
            ref = publisher.publish("registry:5000", "fixture", "v0.0.0", b"compiled-binary")
        self.assertEqual(ref, "registry:5000/fixture@" + publisher.digest(manifest[0]))
        with tarfile.open(fileobj=io.BytesIO(gzip.decompress(uploads[0]))) as archive:
            self.assertEqual(archive.extractfile("walk-app").read(), b"compiled-binary")
            self.assertEqual(archive.getmember("walk-app").mode, 0o755)
        config = json.loads(uploads[1])
        self.assertEqual(config["config"]["Entrypoint"], ["/walk-app"])
        self.assertEqual(config["rootfs"]["diff_ids"], [publisher.digest(gzip.decompress(uploads[0]))])


if __name__ == "__main__":
    unittest.main()
