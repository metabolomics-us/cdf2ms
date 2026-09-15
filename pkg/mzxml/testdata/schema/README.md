# mzXML 3.2 schema set (recovered copy)

These are the official mzXML 3.2 schema files, byte-for-byte apart from the two
repairs listed below. They are the normative reference the `pkg/mzxml` writer is
tested against (`schema_test.go` derives the required attribute sets, permitted
enumeration values, and child-element order from them at test time, and
`scripts/validate_ms_outputs.py` validates generated documents with lxml).

## Where they came from

The canonical location is

    http://sashimi.sourceforge.net/schema_revision/mzXML_3.2/

which now answers **404 for every file** — the mzXML project no longer serves its
own normative schema. The copies here were recovered from archived snapshots of
those exact URLs by `scripts/fetch_mzxml_schema.py`, which pins the snapshot
timestamp of each file:

| file | snapshot |
| --- | --- |
| `mzXML_3.2.xsd` | 20120210053846 |
| `mzXML_idx_3.2.xsd` | 20120211084304 |
| `general_types_1.0.xsd` | 20120211084259 |
| `separation_technique_1.0.xsd` | 20120211072657 |

Re-run `python3 scripts/fetch_mzxml_schema.py` to refetch and re-verify. The
include chain that actually matters is

    mzXML_idx_3.2.xsd -> mzXML_3.2.xsd -> { separation_technique_1.0.xsd,
                                            general_types_1.0.xsd }

The release directory also carries `mzXML_idx_separation_1.0.xsd`, an
illustrative combination that pulls in `column_separation_1.0.xsd`. Nothing in
the validation chain includes it and the archive does not serve that companion
file, so it is deliberately excluded and the committed set stays closed and
loadable.

## Repairs applied (neither changes what a document must look like)

1. **Self-contained includes.** `xs:include schemaLocation="http://sashimi.sourceforge.net/..."`
   pointed at the dead host, so the absolute prefixes were rewritten to relative
   file names.
2. **`general_types_1.0.xsd` had no `targetNamespace`.** `mzXML_3.2.xsd` refers to
   the types it defines through the `cff:` prefix, which is bound to the mzXML 3.2
   namespace, so `cff:namevalueType` and `cff:ontologyEntryType` do not resolve as
   published and the schema set cannot be loaded. The targetNamespace was added,
   which is what the referencing schema already assumes.

## Two things this schema settles about "mzXML 3.2"

* **The spectrum element is still called `<scan>`.** The published 3.2 schema has
  no `<spectrum>` element at all; `<spectrum>` belongs to mzML. Writers that emit
  `<spectrum>` in a `mzXML_3.2` namespace are not producing mzXML. ProteoWizard's
  reference serializer (`pwiz/data/msdata/Serializer_mzXML.cpp`) emits `<scan>`
  too.
* **The indexed envelope is `<index>` / `<indexOffset>` / `<sha1>` inside the root
  `<mzXML>` element** — not the `<indexedListmzXML>` / `<indexList>` /
  `<indexListOffset>` / `<fileChecksum>` envelope that circulates in some
  documentation, which does not exist in this schema. `mzXML_idx_3.2.xsd` also
  defines `<indexOffset>` as required, so a document without an index must write
  it as `xsi:nil="true"` rather than leave it out, and `<sha1>` is restricted to
  exactly 40 characters covering the file "from the beginning of the file up to
  (and including) the opening tag of sha1".
