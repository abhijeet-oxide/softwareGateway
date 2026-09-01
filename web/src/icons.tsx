// THE SINGLE UI ICON REGISTRY. Every general-purpose icon this application
// renders is chosen HERE and nowhere else: change one mapping in this file and
// it changes on every screen.
//
// # Why a registry
//
// This application was drawing from five icon libraries at once - Ant Design's
// own outlined set, Material Symbols, Fluent MDL2, MDI and Lucide - picked one
// call site at a time. Each of those is a coherent family on its own and none
// of them agree with each other about stroke weight, corner radius, optical
// size or how much of a shape to draw, so a toolbar assembled from three of
// them reads as three different products. Worse, Ant Design's outlined set is
// drawn on a 1024 grid in a filled, hairline-thin style that dates the
// interface the moment it sits beside anything contemporary.
//
// So there is one family, and it is Phosphor: a 1.5-weight, round-jointed,
// 16-on-a-24-grid set whose proportions sit naturally beside the platform's own
// symbols. It is also what the sibling tool on this platform uses, which means
// a person moving between the two reads the same glyph for the same idea.
//
// # Why the Ant Design names
//
// The exports keep the familiar `SyncOutlined` / `CheckCircleFilled` spellings,
// so a call site changes its IMPORT and nothing else - this file swapped five
// libraries for one without touching a single piece of layout. The names are a
// vocabulary, not a claim about where the drawing comes from.
//
// # What is NOT here
//
// Vendor marks and artifact-kind marks live in `components/icons.tsx`. A JFrog
// logo or a Helm wheel is an IDENTITY, not a UI glyph: it must look like the
// thing it names, so it comes from the brand sets and is chosen by what the
// object IS rather than by this file.
//
// The glyphs are compiled INTO the bundle by unplugin-icons rather than fetched
// at runtime, which is what keeps an air-gapped deployment from rendering a
// page full of empty boxes.

import type { CSSProperties, FunctionComponent, MouseEventHandler, SVGProps } from 'react'

import PhArrowClockwise from '~icons/ph/arrow-clockwise'
import PhArrowLeft from '~icons/ph/arrow-left'
import PhArrowRight from '~icons/ph/arrow-right'
import PhArrowSquareOut from '~icons/ph/arrow-square-out'
import PhArrowUp from '~icons/ph/arrow-up'
import PhArrowsClockwise from '~icons/ph/arrows-clockwise'
import PhArrowsLeftRight from '~icons/ph/arrows-left-right'
import PhBell from '~icons/ph/bell'
import PhCaretDown from '~icons/ph/caret-down'
import PhCaretRight from '~icons/ph/caret-right'
import PhCertificate from '~icons/ph/certificate'
import PhChartBar from '~icons/ph/chart-bar'
import PhCheck from '~icons/ph/check'
import PhCheckCircle from '~icons/ph/check-circle'
import PhCheckCircleFill from '~icons/ph/check-circle-fill'
import PhCircleHalf from '~icons/ph/circle-half'
import PhClock from '~icons/ph/clock'
import PhClockCounterClockwise from '~icons/ph/clock-counter-clockwise'
import PhCloudArrowDown from '~icons/ph/cloud-arrow-down'
import PhCopy from '~icons/ph/copy'
import PhCube from '~icons/ph/cube'
import PhDatabase from '~icons/ph/database'
import PhDotsSixVertical from '~icons/ph/dots-six-vertical'
import PhDotsThreeVertical from '~icons/ph/dots-three-vertical'
import PhDownloadSimple from '~icons/ph/download-simple'
import PhFileText from '~icons/ph/file-text'
import PhFileZip from '~icons/ph/file-zip'
import PhFolder from '~icons/ph/folder'
import PhBookOpen from '~icons/ph/book-open'
import PhBroadcast from '~icons/ph/broadcast'
import PhFunnel from '~icons/ph/funnel'
import PhHardDrives from '~icons/ph/hard-drives'
import PhLightning from '~icons/ph/lightning'
import PhListBullets from '~icons/ph/list-bullets'
import PhMagnifyingGlass from '~icons/ph/magnifying-glass'
import PhMinus from '~icons/ph/minus'
import PhMinusCircle from '~icons/ph/minus-circle'
import PhPackage from '~icons/ph/package'
import PhPause from '~icons/ph/pause'
import PhPlayCircle from '~icons/ph/play-circle'
import PhPlugsConnected from '~icons/ph/plugs-connected'
import PhProhibit from '~icons/ph/prohibit'
import PhQuestion from '~icons/ph/question'
import PhRocketLaunch from '~icons/ph/rocket-launch'
import PhScales from '~icons/ph/scales'
import PhSealCheck from '~icons/ph/seal-check'
import PhGear from '~icons/ph/gear'
import PhGithubLogo from '~icons/ph/github-logo'
import PhShieldCheck from '~icons/ph/shield-check'
import PhSpinner from '~icons/ph/spinner'
import PhSquaresFour from '~icons/ph/squares-four'
import PhStack from '~icons/ph/stack'
import PhStorefront from '~icons/ph/storefront'
import PhTrash from '~icons/ph/trash'
import PhTrayArrowDown from '~icons/ph/tray-arrow-down'
import PhTreeStructure from '~icons/ph/tree-structure'
import PhWarning from '~icons/ph/warning'
import PhWarningCircle from '~icons/ph/warning-circle'
import PhWarningCircleFill from '~icons/ph/warning-circle-fill'
import PhXCircle from '~icons/ph/x-circle'
import PhXCircleFill from '~icons/ph/x-circle-fill'

type Glyph = FunctionComponent<SVGProps<SVGSVGElement>>

export interface AppIconProps {
  className?: string
  style?: CSSProperties
  onClick?: MouseEventHandler<HTMLSpanElement>
  /** turn continuously; the loading and sync glyphs default to on */
  spin?: boolean
  title?: string
}

/**
 * Wrap a drawing so it behaves like an icon the component library recognises.
 *
 * The `span.anticon > svg` shape is not decoration: Ant Design lays out a
 * button, a menu item, a tag and an alert AROUND `.anticon`, and that class is
 * what carries the gap between a glyph and the label beside it. An icon without
 * it sits a few pixels off from every built-in one on the same row - small
 * enough to read as a mistake rather than as a difference.
 */
function make(slug: string, Drawing: Glyph, spinDefault = false) {
  function AppIcon({ className, style, onClick, spin = spinDefault, title }: AppIconProps) {
    const cls = [`anticon anticon-${slug}`, spin ? 'ui-spin' : '', className]
      .filter(Boolean)
      .join(' ')
    return (
      <span role="img" aria-label={slug} title={title} onClick={onClick} className={cls} style={style}>
        <Drawing width="1em" height="1em" />
      </span>
    )
  }
  AppIcon.displayName = slug
  return AppIcon
}

// --- navigation and structure ----------------------------------------------
export const AppstoreOutlined = make('appstore', PhSquaresFour)
export const DashboardOutlined = make('dashboard', PhSquaresFour)
export const ProductOutlined = make('product', PhCube)
export const PackageOutlined = make('package', PhPackage)
export const InboxOutlined = make('inbox', PhTrayArrowDown)
export const DatabaseOutlined = make('database', PhDatabase)
export const HddOutlined = make('hdd', PhHardDrives)
export const BarChartOutlined = make('bar-chart', PhChartBar)
export const HistoryOutlined = make('history', PhClockCounterClockwise)
export const SettingOutlined = make('setting', PhGear)
export const BellOutlined = make('bell', PhBell)
export const ShopOutlined = make('shop', PhStorefront)
export const ApiOutlined = make('api', PhPlugsConnected)
export const PartitionOutlined = make('partition', PhTreeStructure)
export const ClusterOutlined = make('cluster', PhStack)
export const UnorderedListOutlined = make('list', PhListBullets)
export const BookOutlined = make('book', PhBookOpen)
// Discovery: a sweep going out and listening for what comes back, which is
// what polling a vendor registry on a schedule actually is.
export const RadarChartOutlined = make('radar', PhBroadcast)
// The one vendor mark in this file, and it earns the exception: "managed in
// Git" is a claim about WHERE something lives, and the mark says that faster
// than the word does.
export const GithubOutlined = make('github', PhGithubLogo)

// --- arrows and movement ----------------------------------------------------
export const ArrowLeftOutlined = make('arrow-left', PhArrowLeft)
export const ArrowRightOutlined = make('arrow-right', PhArrowRight)
export const ArrowUpOutlined = make('arrow-up', PhArrowUp)
export const DownOutlined = make('down', PhCaretDown)
export const RightOutlined = make('right', PhCaretRight)
export const SwapOutlined = make('swap', PhArrowsLeftRight)
export const ExportOutlined = make('export', PhArrowSquareOut)
export const DownloadOutlined = make('download', PhDownloadSimple)
export const CloudDownloadOutlined = make('cloud-download', PhCloudArrowDown)

// --- status -----------------------------------------------------------------
// Two channels, always: an outlined glyph for a state being reported and a
// filled one for a state being asserted. A reader who cannot separate the
// colours still gets the shape.
export const CheckOutlined = make('check', PhCheck)
export const CheckCircleOutlined = make('check-circle', PhCheckCircle)
export const CheckCircleFilled = make('check-circle-fill', PhCheckCircleFill)
export const CloseCircleOutlined = make('close-circle', PhXCircle)
export const CloseCircleFilled = make('close-circle-fill', PhXCircleFill)
export const ExclamationCircleOutlined = make('exclamation-circle', PhWarningCircle)
export const ExclamationCircleFilled = make('exclamation-circle-fill', PhWarningCircleFill)
export const WarningOutlined = make('warning', PhWarning)
export const QuestionCircleOutlined = make('question-circle', PhQuestion)
export const ClockCircleOutlined = make('clock-circle', PhClock)
export const MinusCircleOutlined = make('minus-circle', PhMinusCircle)
export const MinusOutlined = make('minus', PhMinus)
export const StopOutlined = make('stop', PhProhibit)
// Half of something: a transfer where some components were already in place
// and the rest are still moving. A half-filled disc says "partly" in one
// shape, which a word in a tag cannot do at ten pixels.
export const PartialOutlined = make('partial', PhCircleHalf)
export const ThunderboltOutlined = make('thunderbolt', PhLightning)
export const RocketOutlined = make('rocket', PhRocketLaunch)
export const ScaleOutlined = make('scale', PhScales)
// Security wears two different marks on purpose: a shield is the SUBJECT (the
// section, the posture), a sealed check is a CLAIM about one artifact (this
// thing is signed and the signature verifies).
export const SafetyOutlined = make('safety', PhShieldCheck)
export const SafetyCertificateOutlined = make('safety-certificate', PhSealCheck)
export const SignatureOutlined = make('signature', PhCertificate)

// Both spin by default: they exist to say that something is in motion, and one
// that had to be told to move was routinely shipped standing still.
export const LoadingOutlined = make('loading', PhSpinner, true)
export const SyncOutlined = make('sync', PhArrowsClockwise, true)

// --- actions ----------------------------------------------------------------
export const ReloadOutlined = make('reload', PhArrowClockwise)
export const SearchOutlined = make('search', PhMagnifyingGlass)
export const CopyOutlined = make('copy', PhCopy)
export const DeleteOutlined = make('delete', PhTrash)
export const MoreOutlined = make('more', PhDotsThreeVertical)
export const HolderOutlined = make('holder', PhDotsSixVertical)
export const FilterOutlined = make('filter', PhFunnel)
export const PlayCircleOutlined = make('play-circle', PhPlayCircle)
export const PauseOutlined = make('pause', PhPause)

// --- files ------------------------------------------------------------------
export const FileTextOutlined = make('file-text', PhFileText)
export const FolderOutlined = make('folder', PhFolder)
export const FolderZip24RegularIcon = make('folder-zip', PhFileZip)
