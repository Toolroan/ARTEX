import type { CollectedArtifact, CollectedArtifactPreview } from "../types";

// Curated library fixtures only; never populated from a real task or capture.
export const artifactFixtures: CollectedArtifact[] = [
  {
    id: "1".repeat(64),
    name: "sample-export.sql",
    category: "database",
    source: "database.example.test",
    description: "Synthetic database export for UI verification.",
    size: 110,
    sha256: "a".repeat(64),
    created_at: Date.parse("2026-11-25T08:40:00Z"),
    worker: "worker-1",
    intent_id: 12,
    original_path: "i12/sample-export.sql",
  },
  {
    id: "2".repeat(64),
    name: "sample-source.tar.gz",
    category: "source",
    source: "app.example.test",
    description: "Synthetic bundled source archive.",
    size: 3 * 1024 * 1024,
    sha256: "b".repeat(64),
    created_at: Date.parse("2026-11-25T08:30:00Z"),
    worker: "worker-2",
    intent_id: 13,
    original_path: "i13/sample-source.tar.gz",
  },
  {
    id: "3".repeat(64),
    name: "sample-config.json",
    category: "config",
    source: "app.example.test",
    size: 58,
    sha256: "c".repeat(64),
    created_at: Date.parse("2026-11-25T08:20:00Z"),
    worker: "worker-2",
    intent_id: 13,
    original_path: "i13/sample-config.json",
  },
  {
    id: "4".repeat(64),
    name: "inert-preview.html",
    category: "evidence",
    source: "web.example.test",
    description: "This preview must remain plain text.",
    size: 144,
    sha256: "d".repeat(64),
    created_at: Date.parse("2026-11-25T08:10:00Z"),
    worker: "worker-1",
    intent_id: 12,
    original_path: "i12/inert-preview.html",
  },
];

export const artifactPreviewFixtures: Record<string, CollectedArtifactPreview> = {
  ["1".repeat(64)]: {
    id: "1".repeat(64),
    name: "sample-export.sql",
    size: 110,
    binary: false,
    content: "-- Synthetic export for UI verification only.\nCREATE TABLE example_fixture (id integer, label text);\n",
  },
  ["2".repeat(64)]: {
    id: "2".repeat(64),
    name: "sample-source.tar.gz",
    size: 3 * 1024 * 1024,
    binary: true,
    too_large: true,
  },
  ["3".repeat(64)]: {
    id: "3".repeat(64),
    name: "sample-config.json",
    size: 58,
    binary: false,
    content: '{"fixture":true,"host":"example.test","enabled":false}\n',
  },
  ["4".repeat(64)]: {
    id: "4".repeat(64),
    name: "inert-preview.html",
    size: 144,
    binary: false,
    content:
      '<h1>Display as text only</h1>\n<script>window.__artifactExecuted = true</script>\n<img src="invalid" onerror="window.__artifactExecuted = true">\n',
  },
};
