"""Runs moto's server with S3's default bucket encryption.

Since January 2023, every S3 bucket has SSE-S3 encryption by default.
DeleteBucketEncryption restores it, so GetBucketEncryption never answers
ServerSideEncryptionConfigurationNotFoundError. moto still answers it, and
the pinned s3-controller panics on that answer (README.md). Arguments pass
to moto_server.
"""

import copy
import sys

from moto.s3 import models
from moto.server import main

DEFAULT_ENCRYPTION = {"Rule": {"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "AES256"}}}

created = models.FakeBucket.__init__


def created_encrypted(self, *args, **kwargs):
    created(self, *args, **kwargs)
    self.encryption = copy.deepcopy(DEFAULT_ENCRYPTION)


def delete_bucket_encryption(self, bucket_name):
    self.get_bucket(bucket_name).encryption = copy.deepcopy(DEFAULT_ENCRYPTION)


models.FakeBucket.__init__ = created_encrypted
models.S3Backend.delete_bucket_encryption = delete_bucket_encryption

if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
