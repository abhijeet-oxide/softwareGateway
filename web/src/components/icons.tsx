import type { CSSProperties, ElementType } from 'react'
import NokiaIcon from '~icons/simple-icons/nokia'
import JFrogIcon from '~icons/logos/jfrog'
import OpenShiftIcon from '~icons/simple-icons/redhatopenshift'
import RedHatIcon from '~icons/simple-icons/redhat'
import DockerIcon from '~icons/simple-icons/docker'
import OciIcon from '~icons/simple-icons/opencontainersinitiative'
import FlaskIcon from '~icons/mdi/flask-outline'
import RocketIcon from '~icons/mdi/rocket-launch-outline'
import ServerIcon from '~icons/mdi/server'
import StoreIcon from '~icons/mdi/store-outline'
import DatabaseIcon from '~icons/mdi/database-outline'
import PackageIcon from '~icons/mdi/package-variant-closed'
import HelmIcon from '~icons/simple-icons/helm'
import FileIcon from '~icons/mdi/file-document-outline'
import AnalyzeIcon from '~icons/mdi/file-tree-outline'
import IndexEditIcon from '@iconify-react/oui/index-edit';
import DownloadIcon from '~icons/mdi/tray-arrow-down'
import LayersIcon from '~icons/mdi/layers-triple-outline'
import SignatureIcon from '~icons/mdi/certificate-outline'
import type { Repository } from '../api/types'
import { branding } from '../theme'
import NokiaNAsset from '../assets/nokia_n.svg'

/**
 * Every icon in the application, chosen in one place.
 *
 * # Why a registry rather than an icon at each call site
 *
 * A registry, a target and an environment are named differently in every
 * deployment - `near`, `primary`, `ocp-prod`, `legacy-dr` - and the icon must
 * follow what the thing IS rather than what somebody called it. Deciding that
 * per call site means ten pages disagreeing about what a Quay mirror looks
 * like.
 *
 * # Why the brand marks
 *
 * A Product Owner reading a location chip is looking for "is this the vendor
 * or is this ours". A vendor's own mark answers that in one glance where a
 * generic box does not - and the vendors here are few and stable, so the marks
 * are worth carrying.
 *
 * Where no honest mark exists the generic one is used rather than a
 * near-enough brand. Quay has no Simple Icons mark; it is a Red Hat product,
 * so it gets Red Hat's, and a registry we cannot identify gets the OCI mark
 * rather than a guess.
 */

export type IconComponent = ElementType<{ style?: CSSProperties }>

/**
 * The mark a deployment puts before the product name.
 *
 * Three shapes, because a company's logo arrives in whichever one they have:
 * an Iconify icon compiled into the bundle, raw SVG markup, or an image by URL.
 * All three are configured in theme.ts and none of them reaches the network at
 * runtime unless the third is pointed at something remote, which an air-gapped
 * deployment should not do.
 *
 * Renders nothing when no mark is set, which is the default: a tool with no
 * logo should look deliberate rather than look like a broken image.
 */
export function BrandMark({ size }: { size?: number }) {
  const mark = branding.mark
  if (!mark) return null
  const px = size ?? branding.markSize

  if (typeof mark === 'string') {
    // Inline SVG takes the colour and size around it; anything else is an
    // image and cannot, so it is sized and left alone.
    if (mark.trim().startsWith('<svg')) {
      return (
        <span
          aria-hidden
          style={{ display: 'inline-flex', width: px, height: px, color: '#fff' }}
          dangerouslySetInnerHTML={{ __html: mark }}
        />
      )
    }
    return <img src={mark} alt="" width={px} height={px} style={{ display: 'block' }} />
  }

  const Mark = mark
  return <Mark aria-hidden style={{ display: 'block', width: px, height: px, color: '#fff' }} />
}

export const NokiaNIcon: IconComponent = (props) => (
  <svg viewBox="0 0 48 48" {...props}>
    <image href={NokiaNAsset} width="48" height="48" />
  </svg>
)

/** Vendors we can name, matched against the source's declared `vendor` layout. */
const VENDOR_MARKS: Record<string, IconComponent> = {
  near: NokiaNIcon,
  nokia: NokiaNIcon,
}

/** Registry types, from `Repository.type`. */
const REGISTRY_MARKS: Record<string, IconComponent> = {
  artifactory: JFrogIcon,
  quay: RedHatIcon,
  acr: DockerIcon,
}

/** Matched against a repository NAME when nothing more reliable applies. */
const NAME_HINTS: [RegExp, IconComponent][] = [
  [/jfrog|artifactory/i, JFrogIcon],
  [/openshift|ocp/i, OpenShiftIcon],
  [/quay/i, RedHatIcon],
  [/nokia|near/i, NokiaIcon],
]

/** Environments, from `Repository.environment`. */
const ENVIRONMENT_MARKS: Record<string, IconComponent> = {
  production: RocketIcon,
  prod: RocketIcon,
  lab: FlaskIcon,
  dev: FlaskIcon,
  test: FlaskIcon,
  staging: ServerIcon,
}

/**
 * The icon for one source or target.
 *
 * Order is deliberate and runs from most reliable to least: what the vendor
 * says it is, then what the registry protocol is, then what the environment
 * is, and only then the name - which is the one an operator can rename on a
 * whim.
 */
export function repositoryIcon(repo: Repository | undefined): IconComponent {
  if (!repo) return OciIcon

  const vendor = repo.vendor?.toLowerCase()
  if (vendor && VENDOR_MARKS[vendor]) return VENDOR_MARKS[vendor]!

  const type = repo.type?.toLowerCase()
  if (type && REGISTRY_MARKS[type]) return REGISTRY_MARKS[type]!

  const repositoryDetails = [repo.name, repo.registry, repo.repository].filter(Boolean).join(' ')
  for (const [pattern, mark] of NAME_HINTS) {
    if (pattern.test(repositoryDetails)) return mark
  }

  const env = repo.environment?.toLowerCase()
  if (env && ENVIRONMENT_MARKS[env]) return ENVIRONMENT_MARKS[env]!

  return repo.role === 'source' ? StoreIcon : DatabaseIcon
}

/** The icon for a location chip, which knows a kind rather than a repository. */
export function locationIcon(
  kind: 'vendor' | 'storage' | 'mirror' | 'production',
  name?: string,
): IconComponent {
  if (name) {
    for (const [pattern, mark] of NAME_HINTS) {
      if (pattern.test(name)) return mark
    }
  }
  return {
    vendor: StoreIcon,
    storage: DatabaseIcon,
    mirror: OpenShiftIcon,
    production: RocketIcon,
  }[kind]
}

/** The icon for an environment on its own. */
export function environmentIcon(environment: string | undefined): IconComponent | undefined {
  const key = environment?.toLowerCase()
  return key ? ENVIRONMENT_MARKS[key] : undefined
}

/**
 * Artifact kinds, for the Contents tiles and the per-type lists.
 *
 * A container image gets Docker's mark and a chart gets Helm's, because those
 * are what the things ARE - a Product Owner recognises them from every other
 * tool they use, and a generic "layers" glyph teaches nothing. Files have no
 * ecosystem mark, so they get a document.
 */
export const ARTIFACT_ICONS = {
  Images: DockerIcon,
  'Helm Charts': HelmIcon,
  Files: FileIcon,
  // The two structural kinds. An index is a table of contents - it names the
  // release's parts and carries none of them - and a signature is an
  // attestation, which is what a certificate mark says everywhere else.
  Index: IndexEditIcon,
  Signatures: SignatureIcon,
} as const

export type ArtifactKind = keyof typeof ARTIFACT_ICONS

export { AnalyzeIcon, DownloadIcon, IndexEditIcon, LayersIcon, SignatureIcon, PackageIcon, NokiaIcon, JFrogIcon, OpenShiftIcon, RocketIcon, FlaskIcon, OciIcon, DockerIcon, HelmIcon }

/**
 * Renders one of the above at text size.
 *
 * A wrapper because every icon needs the same three things - a size that
 * matches the line it sits on, vertical alignment against the text baseline,
 * and a title so it is not a bare glyph to a screen reader.
 */
export function Icon({
  as: Component, size = '1em', colour, title, className,
}: {
  as: IconComponent
  size?: number | string
  colour?: string
  title?: string
  /**
   * Passed through so a caller can adopt Ant Design's own icon class.
   *
   * The nav needs it: Ant Design lays a menu item out around `.anticon`, which
   * carries the gap between the mark and the label. An icon without it sat a
   * few pixels off from the seven built-in ones on either side of it - small
   * enough to look like a mistake rather than a difference.
   */
  className?: string
}) {
  return (
    <span
      role="img"
      aria-label={title}
      title={title}
      className={className}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        verticalAlign: '-0.15em',
        color: colour,
      }}
    >
      <Component style={{ width: size, height: size }} />
    </span>
  )
}
