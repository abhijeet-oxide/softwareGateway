import { useMemo } from 'react'
import { Alert, Descriptions, Modal, Skeleton, Space, Tag, Typography } from 'antd'
import Prism from 'prismjs'
import 'prismjs/components/prism-markup'
import 'prismjs/components/prism-json'
import 'prismjs/components/prism-yaml'
import 'prismjs/components/prism-properties'
import 'prismjs/components/prism-ini'
import 'prismjs/components/prism-bash'
import 'prismjs/components/prism-sql'
import 'prismjs/components/prism-python'
import 'prismjs/themes/prism.css'
import { usePackageFileContent } from '../api/queries'
import { bytes, formatBytes } from '../domain/format'
import { ErrorState } from './layout'
import { mono } from '../theme'

/**
 * Looking at a file a vendor shipped.
 *
 * # Why the interface can show this at all
 *
 * Because analysis already recorded which layers are named files. The content
 * itself is not held here — a release is tens of gigabytes and this system is
 * deliberately not a copy of it — so the bytes are fetched from the vendor
 * registry when somebody asks, through an endpoint that will only serve a
 * digest already recorded as one of THIS release's files.
 *
 * # Why highlighting is a dependency and not a regex
 *
 * The files a reader opens here are configuration: JSON node definitions, XML
 * descriptors, YAML values, properties. Colouring them badly is worse than not
 * colouring them, because a mis-tokenised quote reads as a syntax error in the
 * vendor's file. Prism is compiled into the bundle with the seven grammars
 * these files actually use — nothing is fetched, which the air-gap requires.
 *
 * A file whose extension names no grammar is shown as plain text rather than
 * guessed at.
 */

/** The grammar to read a file with, from what the publisher called it. */
const GRAMMARS: Record<string, string> = {
  json: 'json',
  xml: 'markup', html: 'markup', htm: 'markup', svg: 'markup',
  xsd: 'markup', wsdl: 'markup', pom: 'markup',
  yaml: 'yaml', yml: 'yaml',
  properties: 'properties', conf: 'properties', cfg: 'properties', env: 'properties',
  ini: 'ini', toml: 'ini',
  sh: 'bash', bash: 'bash',
  sql: 'sql',
  py: 'python',
}

function grammarFor(path: string): string | undefined {
  const name = path.split('/').pop() ?? path
  const ext = name.includes('.') ? name.split('.').pop()!.toLowerCase() : ''
  return GRAMMARS[ext]
}

/**
 * The text to show, and whether we reformatted it.
 *
 * A vendor's generated JSON arrives as one line of forty thousand characters,
 * which is not a file anybody can read. Indenting it is the difference between
 * a viewer and a wall — and it is SAID, because the reader is looking at
 * something that is not byte-for-byte what the registry served.
 *
 * Only when it is genuinely one line: a file the publisher already formatted is
 * shown exactly as they wrote it.
 */
function present(content: string, grammar?: string): { text: string; reformatted: boolean } {
  if (grammar !== 'json' || content.includes('\n')) return { text: content, reformatted: false }
  try {
    return { text: JSON.stringify(JSON.parse(content), null, 2), reformatted: true }
  } catch {
    return { text: content, reformatted: false }
  }
}

/** The file, coloured, with a line gutter. */
function Highlighted({ text, grammar }: { text: string; grammar?: string }) {
  const lines = useMemo(() => {
    const language = grammar ? Prism.languages[grammar] : undefined
    const html = language ? Prism.highlight(text, language, grammar!) : escapeHtml(text)
    // Split AFTER highlighting so the grammar sees the whole document —
    // tokenising line by line breaks every multi-line string and comment.
    return html.split('\n')
  }, [text, grammar])

  return (
    <div
      style={{
        display: 'flex', gap: 12, maxHeight: '62vh', overflow: 'auto',
        background: '#FAFAFA', border: '1px solid #F0F0F0', borderRadius: 6, padding: '10px 12px',
      }}
    >
      <pre
        aria-hidden
        style={{
          margin: 0, fontFamily: mono, fontSize: 12, lineHeight: '18px',
          color: '#B0B7C3', textAlign: 'right', userSelect: 'none',
        }}
      >
        {lines.map((_, i) => `${i + 1}`).join('\n')}
      </pre>
      <pre
        style={{
          margin: 0, fontFamily: mono, fontSize: 12, lineHeight: '18px',
          whiteSpace: 'pre', flex: 1,
        }}
        dangerouslySetInnerHTML={{ __html: lines.join('\n') }}
      />
    </div>
  )
}

function escapeHtml(s: string): string {
  return s.replace(/[&<>]/g, (c) => (c === '&' ? '&amp;' : c === '<' ? '&lt;' : '&gt;'))
}

export function FileViewer({
  product, reference, repository, path, digest, onClose,
}: {
  product?: string
  reference?: string
  repository?: string
  /** The file being looked at. Absent closes the modal. */
  path?: string
  digest?: string
  onClose: () => void
}) {
  const file = usePackageFileContent(product, reference, digest, repository)
  const grammar = path ? grammarFor(path) : undefined
  const shown = file.data?.content ? present(file.data.content, grammar) : undefined

  return (
    <Modal
      open={Boolean(digest)}
      onCancel={onClose}
      footer={null}
      width={980}
      destroyOnHidden
      title={
        <Space direction="vertical" size={0}>
          <Typography.Text strong style={{ fontFamily: mono }}>{path}</Typography.Text>
          {file.data?.component && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {file.data.component}
            </Typography.Text>
          )}
        </Space>
      }
    >
      {file.isLoading ? (
        <Skeleton active paragraph={{ rows: 8 }} />
      ) : file.isError ? (
        <ErrorState error={file.error} retry={() => void file.refetch()} />
      ) : file.data?.tooLarge ? (
        <Alert
          type="info"
          showIcon
          message="This file is too large to open here"
          description={
            `It is ${formatBytes(bytes(file.data.sizeBytes)) ?? 'larger'}, and this view reads up to `
            + `${formatBytes(file.data.limit ?? 0) ?? '2 MB'}. Looking at it means pulling the release.`
          }
        />
      ) : file.data?.binary ? (
        <Alert
          type="info"
          showIcon
          message="This file is not text"
          description={
            'The bytes are not valid text — an archive, an image or a compiled artifact. '
            + 'It is shown as what it is rather than as a screen of replacement characters.'
          }
        />
      ) : (
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Descriptions size="small" column={3}>
            <Descriptions.Item label="Size">
              {formatBytes(bytes(file.data?.sizeBytes)) ?? '—'}
            </Descriptions.Item>
            <Descriptions.Item label="Type">
              {grammar ? <Tag>{grammar === 'markup' ? 'xml' : grammar}</Tag> : <Tag>text</Tag>}
            </Descriptions.Item>
            <Descriptions.Item label="Digest">
              <Typography.Text
                copyable={{ text: file.data?.digest }}
                style={{ fontFamily: mono, fontSize: 11 }}
              >
                {(file.data?.digest ?? '').slice(0, 19)}…
              </Typography.Text>
            </Descriptions.Item>
          </Descriptions>

          {shown?.reformatted && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Indented for reading — the publisher shipped it on one line.
            </Typography.Text>
          )}

          <Highlighted text={shown?.text ?? ''} grammar={grammar} />
        </Space>
      )}
    </Modal>
  )
}
