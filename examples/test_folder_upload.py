"""Tests for folder_upload.py's diff logic. Standard library only:

    python3 -m unittest discover -s examples
"""

import unittest

from folder_upload import IncompleteListing, LocalFile, format_rfc3339, parse_tree, plan


def tree(*lines):
    """Remote tree listing entries, as GET /api/v1/tree/... streams them."""
    return [dict(zip(("path", "type", "size", "mtime"), line)) for line in lines]


class PlanTest(unittest.TestCase):
    def test_file_with_same_size_and_mtime_is_skipped(self):
        p = plan(
            [LocalFile("a.txt", 3, 1614855967_000000000)],
            [],
            tree(("a.txt", "file", 3, "2021-03-04T11:06:07Z")),
        )
        self.assertEqual(p.skip, ["a.txt"])
        self.assertEqual(p.upload, [])

    def test_missing_file_is_uploaded(self):
        f = LocalFile("sub/b.txt", 5, 1614855967_000000000)
        p = plan([f], [], tree(("sub", "folder", 0, "2021-03-04T11:06:07Z")))
        self.assertEqual(p.upload, [f])

    def test_file_that_differs_is_a_conflict_not_an_upload(self):
        # The service never overwrites; uploading would only be refused (409).
        p = plan(
            [LocalFile("a.txt", 4, 1614855967_000000000)],
            [],
            tree(("a.txt", "file", 3, "2021-03-04T11:06:07Z")),
        )
        self.assertEqual(p.conflict, ["a.txt"])
        self.assertEqual(p.upload, [])

    def test_mtime_compares_to_the_second_in_any_offset(self):
        # The server reports nanoseconds in its own zone; 11:06:07.9Z == 06:06:07.9-05:00.
        p = plan(
            [LocalFile("a.txt", 3, 1614855967_900000000)],
            [],
            tree(("a.txt", "file", 3, "2021-03-04T06:06:07.900000123-05:00")),
        )
        self.assertEqual(p.skip, ["a.txt"])

    def test_only_empty_folders_missing_remotely_are_created(self):
        # "full" gets created by its file's Upload; "there" already exists.
        p = plan(
            [LocalFile("full/x.txt", 1, 0)],
            ["empty", "full", "there", "nested", "nested/leaf"],
            tree(("there", "folder", 0, "2021-03-04T11:06:07Z")),
        )
        self.assertEqual(p.mkdir, ["empty", "nested/leaf"])

    def test_local_names_match_the_servers_nfc_names(self):
        # macOS may hand out "café" decomposed (NFD); the server stores NFC.
        p = plan(
            [LocalFile("café.txt", 3, 1614855967_000000000)],
            ["résumé"],
            tree(
                ("café.txt", "file", 3, "2021-03-04T11:06:07Z"),
                ("résumé", "folder", 0, "2021-03-04T11:06:07Z"),
            ),
        )
        self.assertEqual(p.skip, ["café.txt"])
        self.assertEqual(p.mkdir, [])

    def test_a_remote_file_where_a_local_folder_is_is_a_conflict(self):
        p = plan([], ["x"], tree(("x", "file", 1, "2021-03-04T11:06:07Z")))
        self.assertEqual(p.conflict, ["x"])
        self.assertEqual(p.mkdir, [])


class FormatTest(unittest.TestCase):
    def test_mtime_is_sent_in_utc_with_nanoseconds(self):
        self.assertEqual(format_rfc3339(1614855967_000000123), "2021-03-04T11:06:07.000000123Z")


class ParseTreeTest(unittest.TestCase):
    def test_reads_one_entry_per_line(self):
        lines = [
            b'{"path":"a","type":"folder","size":0,"mtime":"2021-03-04T11:06:07Z"}',
            b"",
            b'{"path":"a/b.txt","type":"file","size":3,"mtime":"2021-03-04T11:06:07Z"}',
        ]
        self.assertEqual([e["path"] for e in parse_tree(lines)], ["a", "a/b.txt"])

    def test_an_error_line_means_the_listing_is_incomplete(self):
        # Diffing against a partial listing would re-upload files that are there.
        lines = [
            b'{"path":"a","type":"folder","size":0,"mtime":"2021-03-04T11:06:07Z"}',
            b'{"error":"listing failed partway; it is incomplete"}',
        ]
        with self.assertRaises(IncompleteListing):
            list(parse_tree(lines))


if __name__ == "__main__":
    unittest.main()
