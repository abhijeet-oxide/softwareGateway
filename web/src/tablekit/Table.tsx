// The glyphs come from this folder rather than from an icon package: see
// `icons.tsx`. The names below are the ones the drawings are OF, not the ones
// one particular library happens to publish them under.
import {
  ColumnWidthIcon,
  DocumentIcon,
  ExportIcon,
  HolderIcon,
  MoveIcon,
  PinFilledIcon,
  PinIcon,
  ResetIcon,
  SearchIcon,
  SheetIcon,
  VisibilityIcon,
} from "./icons";
import type { MenuProps, TableProps } from "antd";
import {
  Table as AntTable,
  Button,
  Checkbox,
  Dropdown,
  Empty,
  Input,
  message,
  Space,
  Tag,
  Tooltip,
  Typography,
} from "antd";
import type { ColumnsType, ColumnType } from "antd/es/table";
import React from "react";

import styles from "./Table.module.css";

type AnyRecord = Record<string, any>;
type DropSide = "left" | "right";
type PinSide = "left" | "right";
type PinStateValue = PinSide | null;

/**
 * Every class `Table.module.css` defines, written out.
 *
 * Not decoration: one of the two repositories that share this folder compiles
 * with `noUncheckedIndexedAccess`, under which an index signature yields
 * `string | undefined` - so `s.wrapper` would be a possibly-undefined class
 * name at every one of its hundred call sites. Naming the keys makes `s` an
 * ordinary object with ordinary properties, and the compiler then also catches
 * the real mistake this guards against: a class renamed in the stylesheet and
 * left behind in the component, which fails silently as an unstyled element.
 */
type StyleName =
  | "activeBadge"
  | "borderedHeader"
  | "columnSearch"
  | "columnVisibilityCount"
  | "columnVisibilityDropdown"
  | "columnVisibilityEmpty"
  | "columnVisibilityFooter"
  | "columnVisibilityHeader"
  | "columnVisibilityItem"
  | "columnVisibilityLabel"
  | "columnVisibilityList"
  | "columnVisibilityOverlay"
  | "columnVisibilityPin"
  | "control"
  | "controlsAlways"
  | "densityComfortable"
  | "densityCompact"
  | "densityMiddle"
  | "dragButton"
  | "headerDragOverLeft"
  | "headerDragOverRight"
  | "headerHover"
  | "headerRecentlyMoved"
  | "headerResizing"
  | "pinIndicator"
  | "preferenceDropdown"
  | "reorderGuide"
  | "reorderTooltip"
  | "resizeGuide"
  | "resizeHandle"
  | "titleRoot"
  | "titleText"
  | "toolbar"
  | "toolbarDropdown"
  | "wrapper";

const s = styles as Record<StyleName, string>;

function cx(...values: Array<string | false | null | undefined>) {
  return values.filter(Boolean).join(" ");
}

export type TableEnhancedState = {
  widths: Record<string, number>;
  order: string[];
  pinned?: Record<string, PinStateValue>;
  hidden?: string[];
};

export type TableEnhancedActions = {
  resetLayout: () => void;
  resetColumnWidth: (columnKey: string) => void;
  resetColumnOrder: () => void;
  pinColumn: (columnKey: string, side: PinSide) => void;
  unpinColumn: (columnKey: string) => void;
  setColumnVisible: (columnKey: string, visible: boolean) => void;
  autoFitColumn: (columnKey: string) => void;
  autoFitTable: () => void;
  getState: () => TableEnhancedState;
  setState: (state: TableEnhancedState) => void;
};

export type TableEnhancedColumn<RecordType> = ColumnType<RecordType> & {
  disableResize?: boolean;
  disableReorder?: boolean;
  resizable?: boolean;
  reorderable?: boolean;
  children?: TableEnhancedColumns<RecordType>;
};

export type TableEnhancedColumns<RecordType> =
  TableEnhancedColumn<RecordType>[];

export type TableEnhancedProps<RecordType extends AnyRecord = AnyRecord> =
  TableProps<RecordType> & {
    tableEnhancedKey?: string;
    tableEnhancedActionsRef?: React.MutableRefObject<TableEnhancedActions | null>;
    enableColumnResize?: boolean;
    enableColumnReorder?: boolean;
    allow_export?: boolean;
    show_column_visibility?: boolean;
    tableEnhancedDebug?: boolean;
    tableEnhancedShowActiveBadge?: boolean;
    minColumnWidth?: number;
    defaultColumnWidth?: number;
    showColumnControls?: "always" | "hover" | "off";
    tableEnhancedDensity?: "comfortable" | "middle" | "compact";
    tableEnhancedBorderedHeader?: boolean;
    storage?: Storage;

    onTableEnhancedColumnResize?: (columnKey: string, width: number) => void;
    onTableEnhancedColumnReorder?: (order: string[]) => void;
    onTableEnhancedColumnPin?: (
      columnKey: string,
      side: PinSide | null,
    ) => void;
    onTableEnhancedColumnVisibilityChange?: (
      columnKey: string,
      visible: boolean,
    ) => void;
  };

const STORAGE_PREFIX = "antd-table-enhanced";
const STORAGE_WRITE_DEBOUNCE_MS = 240;
const REORDER_TOOLTIP_CLASS = "antd-table-enhanced-reorder-tooltip";

/**
 * Keeps AntD Dropdown overlays above Modal, Drawer, Mask, etc.
 * AntD Modal default z-index is usually 1000.
 */
const DROPDOWN_OVERLAY_Z_INDEX = 9999;

function getDefaultDropdownPopupContainer(
  triggerNode: HTMLElement,
): HTMLElement {
  if (!canUseDOM()) return triggerNode;
  // Render in body to escape any parent stacking context / overflow:hidden.
  return document.body;
}

let activeDragColumnKey: string | null = null;
let activeDragColumnLabel: string | null = null;

function canUseDOM() {
  return typeof window !== "undefined" && typeof document !== "undefined";
}

function useIsomorphicLayoutEffect(
  effect: React.EffectCallback,
  deps?: React.DependencyList,
) {
  const effectToUse = canUseDOM() ? React.useLayoutEffect : React.useEffect;
  effectToUse(effect, deps);
}

function getDefaultStorage(): Storage | undefined {
  if (!canUseDOM()) return undefined;

  try {
    return window.localStorage;
  } catch {
    return undefined;
  }
}

function isDebugEnabled(explicit?: boolean) {
  if (explicit) return true;
  if (!canUseDOM()) return false;

  try {
    return window.localStorage.getItem("antd-table-enhanced-debug") === "1";
  } catch {
    return false;
  }
}

function debugLog(debug?: boolean, ...args: any[]) {
  if (isDebugEnabled(debug)) console.log("[antd-table-enhanced]", ...args);
}

function debugWarn(debug?: boolean, ...args: any[]) {
  if (isDebugEnabled(debug)) console.warn("[antd-table-enhanced]", ...args);
}

function emptyState(): TableEnhancedState {
  return {
    widths: {},
    order: [],
    pinned: {},
    hidden: [],
  };
}

function hasMeaningfulState(state: TableEnhancedState) {
  return (
    Object.keys(state.widths ?? {}).length > 0 ||
    (state.order ?? []).length > 0 ||
    Object.keys(state.pinned ?? {}).length > 0 ||
    (state.hidden ?? []).length > 0
  );
}

function statesEqual(a: TableEnhancedState, b: TableEnhancedState) {
  const aw = a.widths ?? {};
  const bw = b.widths ?? {};
  const ap = a.pinned ?? {};
  const bp = b.pinned ?? {};
  const ah = a.hidden ?? [];
  const bh = b.hidden ?? [];

  const awKeys = Object.keys(aw);
  const bwKeys = Object.keys(bw);
  const apKeys = Object.keys(ap);
  const bpKeys = Object.keys(bp);

  if (awKeys.length !== bwKeys.length) return false;
  if ((a.order ?? []).length !== (b.order ?? []).length) return false;
  if (apKeys.length !== bpKeys.length) return false;
  if (ah.length !== bh.length) return false;

  for (const key of awKeys) {
    if (aw[key] !== bw[key]) return false;
  }

  for (let i = 0; i < (a.order ?? []).length; i += 1) {
    if (a.order[i] !== b.order[i]) return false;
  }

  for (const key of apKeys) {
    if (ap[key] !== bp[key]) return false;
  }

  const ahSet = new Set(ah);
  for (const key of bh) {
    if (!ahSet.has(key)) return false;
  }

  return true;
}

function sanitizePersistedState(
  value: any,
  validWidthKeys?: string[],
  validOrderKeys?: string[],
): TableEnhancedState {
  const widthKeySet = validWidthKeys ? new Set(validWidthKeys) : undefined;
  const orderKeySet = validOrderKeys ? new Set(validOrderKeys) : undefined;

  const widths: Record<string, number> = {};

  if (value?.widths && typeof value.widths === "object") {
    Object.entries(value.widths).forEach(([key, rawWidth]) => {
      const width = Number(rawWidth);
      if (!Number.isFinite(width) || width <= 0) return;
      if (widthKeySet && !widthKeySet.has(key)) return;
      widths[key] = Math.round(width);
    });
  }

  const order: string[] = [];
  const seenOrder = new Set<string>();

  if (Array.isArray(value?.order)) {
    value.order.forEach((rawKey: unknown) => {
      const key = String(rawKey);
      if (!key || seenOrder.has(key)) return;
      if (orderKeySet && !orderKeySet.has(key)) return;
      seenOrder.add(key);
      order.push(key);
    });
  }

  const pinned: Record<string, PinStateValue> = {};

  if (value?.pinned && typeof value.pinned === "object") {
    Object.entries(value.pinned).forEach(([key, rawSide]) => {
      if (orderKeySet && !orderKeySet.has(key)) return;

      if (rawSide === "left" || rawSide === "right") {
        pinned[key] = rawSide;
        return;
      }

      if (rawSide === null) {
        pinned[key] = null;
      }
    });
  }

  const hidden: string[] = [];
  const seenHidden = new Set<string>();

  if (Array.isArray(value?.hidden)) {
    value.hidden.forEach((rawKey: unknown) => {
      const key = String(rawKey);
      if (!key || seenHidden.has(key)) return;
      if (orderKeySet && !orderKeySet.has(key)) return;
      seenHidden.add(key);
      hidden.push(key);
    });
  }

  return {
    widths,
    order,
    pinned,
    hidden,
  };
}

type AutoFitColumnTarget<RecordType> = {
  key: string;
  title: string;
  column: TableEnhancedColumn<RecordType>;
};

let autoFitMeasureCanvas: HTMLCanvasElement | null = null;

function getAutoFitMeasureContext(): CanvasRenderingContext2D | null {
  if (!canUseDOM()) return null;

  if (!autoFitMeasureCanvas) {
    autoFitMeasureCanvas = document.createElement("canvas");
  }

  return autoFitMeasureCanvas.getContext("2d");
}

function getCssFont(element?: Element | null) {
  if (!canUseDOM() || !element) return "14px Arial";

  const computed = window.getComputedStyle(element);

  if (computed.font) return computed.font;

  return [
    computed.fontStyle,
    computed.fontVariant,
    computed.fontWeight,
    `${computed.fontSize}/${computed.lineHeight}`,
    computed.fontFamily,
  ]
    .filter(Boolean)
    .join(" ");
}

function measurePlainTextWidth(text: string, font: string) {
  const safeText = String(text ?? "");

  const lines = safeText.split(/\r?\n/g);
  const context = getAutoFitMeasureContext();

  if (!context) {
    return Math.max(...lines.map((line) => line.length * 8), 0);
  }

  context.font = font;

  return Math.max(...lines.map((line) => context.measureText(line).width), 0);
}

function getAutoFitRenderedCellText<RecordType extends AnyRecord>(
  column: TableEnhancedColumn<RecordType>,
  record: RecordType,
  rowIndex: number,
) {
  let rawValue = getNestedValue(record, column.dataIndex);

  if (typeof column.render === "function") {
    try {
      const rendered = column.render(rawValue, record, rowIndex) as any;

      rawValue =
        rendered && typeof rendered === "object" && "children" in rendered
          ? rendered.children
          : rendered;
    } catch {
      rawValue = getNestedValue(record, column.dataIndex);
    }
  }

  return normalizeExportCellValue(rawValue);
}

function collectAutoFitLeafTargets<RecordType extends AnyRecord>(
  columns: TableEnhancedColumns<RecordType>,
  indexPath: number[] = [],
): AutoFitColumnTarget<RecordType>[] {
  return columns.flatMap((column, index) => {
    const currentIndexPath = [...indexPath, index];
    const columnKey = getColumnKey(column, currentIndexPath);

    if (Array.isArray(column.children) && column.children.length > 0) {
      return collectAutoFitLeafTargets(
        column.children as TableEnhancedColumns<RecordType>,
        currentIndexPath,
      );
    }

    const titleNode =
      typeof column.title === "function" ? undefined : column.title;

    return [
      {
        key: columnKey,
        title: getColumnDisplayLabel(columnKey, titleNode),
        column,
      },
    ];
  });
}

function findAutoFitTargetsForColumnKey<RecordType extends AnyRecord>(
  columns: TableEnhancedColumns<RecordType>,
  targetColumnKey: string,
  indexPath: number[] = [],
): AutoFitColumnTarget<RecordType>[] {
  for (let index = 0; index < columns.length; index += 1) {
    const column = columns[index];
    // A hole in the array is not a column: skipping is the honest answer, and
    // it is what keeps this loop typed under noUncheckedIndexedAccess.
    if (!column) continue;
    const currentIndexPath = [...indexPath, index];
    const columnKey = getColumnKey(column, currentIndexPath);

    const hasChildren =
      Array.isArray(column.children) && column.children.length > 0;

    if (columnKey === targetColumnKey) {
      if (hasChildren) {
        return collectAutoFitLeafTargets(
          column.children as TableEnhancedColumns<RecordType>,
          currentIndexPath,
        );
      }

      const titleNode =
        typeof column.title === "function" ? undefined : column.title;

      return [
        {
          key: columnKey,
          title: getColumnDisplayLabel(columnKey, titleNode),
          column,
        },
      ];
    }

    if (hasChildren) {
      const childResult = findAutoFitTargetsForColumnKey(
        column.children as TableEnhancedColumns<RecordType>,
        targetColumnKey,
        currentIndexPath,
      );

      if (childResult.length) return childResult;
    }
  }

  return [];
}

function calculateAutoFitColumnWidth<RecordType extends AnyRecord>(options: {
  target: AutoFitColumnTarget<RecordType>;
  dataSource?: readonly RecordType[];
  wrapperElement?: HTMLElement | null;
  minColumnWidth: number;
}) {
  const { target, dataSource, wrapperElement, minColumnWidth } = options;

  const headerCell = wrapperElement?.querySelector(".ant-table-thead th");
  const bodyCell = wrapperElement?.querySelector(".ant-table-tbody td");

  const headerFont = getCssFont(headerCell ?? wrapperElement);
  const bodyFont = getCssFont(bodyCell ?? wrapperElement);

  const rows = Array.isArray(dataSource) ? dataSource : [];

  const headerWidth = measurePlainTextWidth(target.title, headerFont);

  const maxBodyWidth = rows.reduce((maxWidth, record, rowIndex) => {
    const text = getAutoFitRenderedCellText(target.column, record, rowIndex);

    return Math.max(maxWidth, measurePlainTextWidth(text, bodyFont));
  }, 0);

  const extraWidth = 56;

  return Math.max(
    minColumnWidth,
    Math.ceil(Math.max(headerWidth, maxBodyWidth) + extraWidth),
  );
}

function safeReadStorage(
  key: string,
  storage?: Storage,
  debug?: boolean,
): TableEnhancedState {
  const fallback = emptyState();
  const targetStorage = storage ?? getDefaultStorage();

  if (!targetStorage) return fallback;

  try {
    const raw = targetStorage.getItem(key);
    if (!raw) return fallback;

    const parsed = JSON.parse(raw);
    const state = sanitizePersistedState(parsed);

    debugLog(debug, "Loaded state", key, state);
    return state;
  } catch (error) {
    debugWarn(debug, "Failed to read state", key, error);
    return fallback;
  }
}

function safeWriteStorage(
  key: string,
  value: TableEnhancedState,
  storage?: Storage,
  debug?: boolean,
) {
  const targetStorage = storage ?? getDefaultStorage();
  if (!targetStorage) return;

  try {
    targetStorage.setItem(key, JSON.stringify(value));
    debugLog(debug, "Saved state", key, value);
  } catch (error) {
    debugWarn(debug, "Failed to save state", key, error);
  }
}

function safeRemoveStorage(key: string, storage?: Storage, debug?: boolean) {
  const targetStorage = storage ?? getDefaultStorage();
  if (!targetStorage) return;

  try {
    targetStorage.removeItem(key);
    debugLog(debug, "Removed state", key);
  } catch (error) {
    debugWarn(debug, "Failed to remove state", key, error);
  }
}

function simpleHash(input: string) {
  let hash = 0;

  for (let i = 0; i < input.length; i += 1) {
    hash = Math.imul(31, hash) + input.charCodeAt(i);
    hash |= 0;
  }

  return Math.abs(hash).toString(36);
}

function normalizeDataIndex(dataIndex: unknown): string | undefined {
  if (Array.isArray(dataIndex)) return dataIndex.join(".");

  if (
    typeof dataIndex === "string" ||
    typeof dataIndex === "number" ||
    typeof dataIndex === "symbol"
  ) {
    return String(dataIndex);
  }

  return undefined;
}

function getTitleSignature(title: unknown): string | undefined {
  if (typeof title === "string" || typeof title === "number") {
    return String(title);
  }

  return undefined;
}

function reactNodeToPlainText(node: React.ReactNode): string | undefined {
  if (typeof node === "string" || typeof node === "number") {
    return String(node);
  }

  if (Array.isArray(node)) {
    const text = node
      .map((child) => reactNodeToPlainText(child))
      .filter(Boolean)
      .join(" ")
      .trim();

    return text || undefined;
  }

  if (React.isValidElement(node)) {
    return reactNodeToPlainText((node.props as any)?.children);
  }

  return undefined;
}

function getColumnDisplayLabel(
  columnKey: string,
  columnTitle?: React.ReactNode,
): string {
  return reactNodeToPlainText(columnTitle)?.trim() || columnKey;
}

function getColumnKey<RecordType>(
  column: TableEnhancedColumn<RecordType>,
  indexPath: number[],
): string {
  const keyFromKey =
    column.key !== undefined && column.key !== null
      ? String(column.key)
      : undefined;

  const keyFromDataIndex = normalizeDataIndex(column.dataIndex);
  const keyFromTitle = getTitleSignature(column.title);

  return (
    keyFromKey ??
    keyFromDataIndex ??
    (keyFromTitle ? `title_${simpleHash(keyFromTitle)}` : undefined) ??
    `column_${indexPath.join("_")}`
  );
}

function getTopLevelColumnKeys<RecordType>(
  columns: TableEnhancedColumns<RecordType>,
) {
  return columns.map((column, index) => getColumnKey(column, [index]));
}

function getAllColumnKeys<RecordType>(
  columns: TableEnhancedColumns<RecordType>,
  indexPath: number[] = [],
): string[] {
  return columns.flatMap((column, index) => {
    const currentIndexPath = [...indexPath, index];
    const key = getColumnKey(column, currentIndexPath);

    if (Array.isArray(column.children) && column.children.length > 0) {
      return [
        key,
        ...getAllColumnKeys(
          column.children as TableEnhancedColumns<RecordType>,
          currentIndexPath,
        ),
      ];
    }

    return [key];
  });
}

function numericWidth(width: unknown): number | undefined {
  if (typeof width === "number" && Number.isFinite(width)) return width;

  if (typeof width === "string") {
    const parsed = Number(width.replace("px", "").trim());
    if (Number.isFinite(parsed)) return parsed;
  }

  return undefined;
}

function isResizeEnabledForColumn<RecordType>(
  column: TableEnhancedColumn<RecordType>,
) {
  if (column.disableResize) return false;
  if (column.resizable === false) return false;
  return true;
}

function isReorderEnabledForColumn<RecordType>(
  column: TableEnhancedColumn<RecordType>,
) {
  if (column.fixed) return false;
  if (column.disableReorder) return false;
  if (column.reorderable === false) return false;
  return true;
}

function normalizeFixedColumnPlacement<RecordType>(
  columns: TableEnhancedColumns<RecordType>,
): TableEnhancedColumns<RecordType> {
  const left = columns.filter(
    (column) => column.fixed === "left" || column.fixed === true,
  );
  const middle = columns.filter((column) => !column.fixed);
  const right = columns.filter((column) => column.fixed === "right");

  return [...left, ...middle, ...right];
}

function normalizePersistedOrder(
  persistedOrder: string[],
  currentKeys: string[],
) {
  return [
    ...persistedOrder.filter((key) => currentKeys.includes(key)),
    ...currentKeys.filter((key) => !persistedOrder.includes(key)),
  ];
}

function moveArrayItemByDropSide<T>(
  array: T[],
  fromIndex: number,
  toIndex: number,
  side: DropSide,
) {
  const item = array[fromIndex];
  const target = array[toIndex];
  // Either index out of range means there is nothing to move: hand the array
  // back untouched rather than splicing an undefined into it.
  if (item === undefined || target === undefined) return array;

  const withoutItem = array.filter((_, index) => index !== fromIndex);
  const targetIndexAfterRemoval = withoutItem.indexOf(target);

  const insertIndex =
    side === "right" ? targetIndexAfterRemoval + 1 : targetIndexAfterRemoval;

  const next = [...withoutItem];
  next.splice(insertIndex, 0, item);

  return next;
}

function buildAutoStorageKey<RecordType>(
  columns?: ColumnsType<RecordType>,
  rowKey?: TableProps<RecordType>["rowKey"],
) {
  const pathname = canUseDOM() ? window.location.pathname : "ssr";

  const normalizedColumns = (
    (columns ?? []) as TableEnhancedColumns<RecordType>
  ).map((column, index) => getColumnKey(column, [index]));

  const rowKeyText =
    typeof rowKey === "string"
      ? rowKey
      : typeof rowKey === "function"
        ? "rowKeyFn"
        : "noRowKey";

  const signature = `${pathname}|${rowKeyText}|${normalizedColumns.join("|")}`;

  return `${STORAGE_PREFIX}:${pathname}:${simpleHash(signature)}`;
}

function orderColumns<RecordType>(
  columns: TableEnhancedColumns<RecordType>,
  persistedOrder: string[],
  debug?: boolean,
): TableEnhancedColumns<RecordType> {
  if (!persistedOrder.length) return columns;

  const currentKeys = getTopLevelColumnKeys(columns);
  const normalizedOrder = normalizePersistedOrder(persistedOrder, currentKeys);

  const columnByKey = new Map<string, TableEnhancedColumn<RecordType>>();

  columns.forEach((column, index) => {
    columnByKey.set(getColumnKey(column, [index]), column);
  });

  const reorderableColumnsInSavedOrder = normalizedOrder
    .map((key) => columnByKey.get(key))
    .filter(Boolean)
    .filter((column) =>
      isReorderEnabledForColumn(column as TableEnhancedColumn<RecordType>),
    ) as TableEnhancedColumns<RecordType>;

  const queue = [...reorderableColumnsInSavedOrder];

  const result = columns.map((column) => {
    if (!isReorderEnabledForColumn(column)) return column;
    return queue.shift() ?? column;
  });

  debugLog(debug, "Applied saved order", {
    persistedOrder,
    currentKeys,
    normalizedOrder,
    resultKeys: getTopLevelColumnKeys(result),
  });

  return result;
}

function applyPinnedColumns<RecordType>(
  columns: TableEnhancedColumns<RecordType>,
  pinned: Record<string, PinStateValue> = {},
): TableEnhancedColumns<RecordType> {
  return columns.map((column, index) => {
    const columnKey = getColumnKey(column, [index]);
    const pinValue = pinned[columnKey];

    const nextColumn: TableEnhancedColumn<RecordType> = {
      ...column,
    };

    if (pinValue === "left" || pinValue === "right") {
      nextColumn.fixed = pinValue;
    } else if (pinValue === null) {
      delete nextColumn.fixed;
    }

    return nextColumn;
  });
}

function filterHiddenColumns<RecordType>(
  columns: TableEnhancedColumns<RecordType>,
  hidden: string[] = [],
): TableEnhancedColumns<RecordType> {
  const hiddenSet = new Set(hidden);

  return columns.filter((column, index) => {
    const columnKey = getColumnKey(column, [index]);
    return !hiddenSet.has(columnKey);
  });
}

function getDragColumnLabelFromEvent(
  event: React.DragEvent,
  fallback?: string,
): string {
  return (
    event.dataTransfer.getData(
      "application/x-antd-table-enhanced-column-label",
    ) ||
    activeDragColumnLabel ||
    fallback ||
    "column"
  );
}

function buildReorderTooltipText(options: {
  draggedColumnKey?: string | null;
  draggedColumnLabel: string;
  targetColumnKey: string;
  targetColumnLabel: string;
  previousColumnKey?: string;
  previousColumnLabel?: string;
  side: DropSide;
}) {
  const {
    draggedColumnKey,
    draggedColumnLabel,
    targetColumnKey,
    targetColumnLabel,
    previousColumnKey,
    previousColumnLabel,
    side,
  } = options;

  const columnBeforeKey =
    side === "right" ? targetColumnKey : previousColumnKey;

  const columnBeforeLabel =
    side === "right" ? targetColumnLabel : previousColumnLabel;

  if (
    columnBeforeLabel &&
    columnBeforeKey &&
    columnBeforeKey !== draggedColumnKey
  ) {
    return `Place ${draggedColumnLabel} after ${columnBeforeLabel}`;
  }

  return `Place ${draggedColumnLabel} before ${targetColumnLabel}`;
}

function removeReorderTooltips() {
  if (!canUseDOM()) return;

  document.querySelectorAll(`.${REORDER_TOOLTIP_CLASS}`).forEach((tooltip) => {
    tooltip.remove();
  });
}

function removeReorderGuides() {
  if (!canUseDOM()) return;

  document.querySelectorAll(`.${s.reorderGuide}`).forEach((guide) => {
    guide.remove();
  });

  removeReorderTooltips();
}

function createReorderTooltip() {
  if (!canUseDOM()) return undefined;

  removeReorderTooltips();

  const tooltip = document.createElement("div");
  tooltip.className = cx(REORDER_TOOLTIP_CLASS, s.reorderTooltip);

  // Only what is genuinely dynamic is written here. How the label LOOKS is in
  // the stylesheet with everything else, on the design system's own tokens -
  // written inline it was a black box with a hardcoded shadow, which is the one
  // thing on screen a change of theme could not reach.
  tooltip.style.transform = "translate3d(-9999px, -9999px, 0)";

  document.body.appendChild(tooltip);

  return {
    setText(text: string) {
      tooltip.textContent = text;
    },

    setPosition(x: number, y: number) {
      const offset = 12;
      const margin = 8;

      const rect = tooltip.getBoundingClientRect();

      let nextX = x + offset;
      let nextY = y + offset;

      if (nextX + rect.width > window.innerWidth - margin) {
        nextX = x - rect.width - offset;
      }

      if (nextY + rect.height > window.innerHeight - margin) {
        nextY = y - rect.height - offset;
      }

      tooltip.style.transform = `translate3d(${nextX}px, ${nextY}px, 0)`;
    },

    remove() {
      tooltip.remove();
    },
  };
}

type EnhancedTitleProps = {
  columnKey: string;
  columnLabel: string;
  reorderEnabled: boolean;
  controlsEnabled: boolean;
  pinnedSide?: PinSide;
  debug?: boolean;
  children: React.ReactNode;
};

const EnhancedTitle: React.FC<EnhancedTitleProps> = ({
  columnKey,
  columnLabel,
  reorderEnabled,
  controlsEnabled,
  pinnedSide,
  debug,
  children,
}) => {
  const showHandle = reorderEnabled && controlsEnabled;

  const handleDragStart = (event: React.DragEvent<HTMLElement>) => {
    if (!showHandle) return;

    event.stopPropagation();
    removeReorderGuides();

    activeDragColumnKey = columnKey;
    activeDragColumnLabel = columnLabel;

    event.dataTransfer.effectAllowed = "move";
    event.dataTransfer.setData("text/plain", columnKey);
    event.dataTransfer.setData(
      "application/x-antd-table-enhanced-column",
      columnKey,
    );
    event.dataTransfer.setData(
      "application/x-antd-table-enhanced-column-label",
      columnLabel,
    );

    debugLog(debug, "Drag start", {
      fromColumnKey: columnKey,
      fromColumnLabel: columnLabel,
    });
  };

  const handleDragEnd = () => {
    debugLog(debug, "Drag end", { columnKey });

    activeDragColumnKey = null;
    activeDragColumnLabel = null;

    removeReorderGuides();
  };

  return (
    <span
      className={s.titleRoot}
      data-antd-table-enhanced-title="true"
      data-antd-table-enhanced-column-key={columnKey}
    >
      {pinnedSide ? (
        <Tooltip title={`Pinned to ${pinnedSide}`}>
          <PinFilledIcon className={s.pinIndicator} />
        </Tooltip>
      ) : null}

      <span
        className={s.titleText}
        title={typeof children === "string" ? children : undefined}
      >
        {children}
      </span>

      {showHandle ? (
        <Tooltip title="Drag column" mouseEnterDelay={0.45}>
          <Button
            type="text"
            size="small"
            shape="circle"
            className={cx(s.control, s.dragButton)}
            icon={<HolderIcon />}
            draggable
            aria-label={`Drag to reorder column ${columnLabel}`}
            onDragStart={handleDragStart}
            onDragEnd={handleDragEnd}
            onClick={(event) => {
              event.preventDefault();
              event.stopPropagation();
            }}
            onContextMenu={(event) => {
              event.preventDefault();
              event.stopPropagation();
            }}
          />
        </Tooltip>
      ) : null}
    </span>
  );
};

type HeaderContextMenuPayload = {
  event: React.MouseEvent<HTMLTableCellElement>;
  columnKey: string;
  columnTitle?: React.ReactNode;
};

type HeaderCellProps = React.ThHTMLAttributes<HTMLTableCellElement> & {
  enhancedColumnKey?: string;
  enhancedColumnTitle?: React.ReactNode;
  enhancedColumnLabel?: string;
  enhancedPreviousColumnKey?: string;
  enhancedPreviousColumnLabel?: string;
  enhancedWidth?: number;
  enhancedResizeEnabled?: boolean;
  enhancedReorderEnabled?: boolean;
  enhancedMinColumnWidth?: number;
  enhancedShowColumnControls?: "always" | "hover" | "off";
  enhancedRecentlyMoved?: boolean;
  enhancedDebug?: boolean;

  enhancedContextMenuOpen?: boolean;
  enhancedPreferenceMenuItems?: MenuProps["items"];
  enhancedGetPopupContainer?: (triggerNode: HTMLElement) => HTMLElement;

  enhancedOnContextMenuOpenChange?: (columnKey: string, open: boolean) => void;

  enhancedOnPreferenceMenuClick?: (
    columnKey: string,
    actionKey: string,
  ) => void;

  enhancedOnColumnResize?: (columnKey: string, width: number) => void;

  enhancedOnColumnDrop?: (
    fromColumnKey: string,
    toColumnKey: string,
    side: DropSide,
  ) => void;

  enhancedOnHeaderContextMenu?: (payload: HeaderContextMenuPayload) => void;
};

function getDropSideFromEvent(
  event: React.DragEvent<HTMLTableCellElement>,
): DropSide {
  const rect = event.currentTarget.getBoundingClientRect();
  return event.clientX < rect.left + rect.width / 2 ? "left" : "right";
}

function createResizeGuide() {
  if (!canUseDOM()) return undefined;

  const guide = document.createElement("div");
  guide.className = s.resizeGuide;
  document.body.appendChild(guide);

  return {
    setX(x: number) {
      guide.style.left = `${x}px`;
    },
    remove() {
      guide.remove();
    },
  };
}

function createReorderGuide() {
  if (!canUseDOM()) return undefined;

  removeReorderGuides();

  const guide = document.createElement("div");
  guide.className = s.reorderGuide;
  document.body.appendChild(guide);

  return {
    setX(x: number) {
      guide.style.left = `${x}px`;
    },
    remove() {
      guide.remove();
    },
  };
}

function createHeaderCell(ExistingHeaderCell?: any) {
  function EnhancedHeaderCell(props: HeaderCellProps) {
    const {
      enhancedColumnKey,
      enhancedColumnTitle,
      enhancedColumnLabel,
      enhancedPreviousColumnKey,
      enhancedPreviousColumnLabel,
      enhancedWidth,
      enhancedResizeEnabled,
      enhancedReorderEnabled,
      enhancedMinColumnWidth = 90,
      enhancedShowColumnControls = "hover",
      enhancedRecentlyMoved,
      enhancedDebug,
      enhancedContextMenuOpen,
      enhancedPreferenceMenuItems,
      enhancedGetPopupContainer,
      enhancedOnContextMenuOpenChange,
      enhancedOnPreferenceMenuClick,
      enhancedOnColumnResize,
      enhancedOnColumnDrop,
      enhancedOnHeaderContextMenu,

      className,
      style,
      onMouseEnter,
      onMouseLeave,
      onDragOver,
      onDragLeave,
      onDragEnd,
      onDrop,
      onContextMenu,
      children,
      ...rest
    } = props;

    const [hovered, setHovered] = React.useState(false);
    const [resizing, setResizing] = React.useState(false);
    const [dragOverSide, setDragOverSide] = React.useState<DropSide | null>(
      null,
    );

    const reorderGuideRef = React.useRef<ReturnType<
      typeof createReorderGuide
    > | null>(null);

    const reorderTooltipRef = React.useRef<ReturnType<
      typeof createReorderTooltip
    > | null>(null);

    const controlsEnabled = enhancedShowColumnControls !== "off";

    const cleanupReorderGuide = React.useCallback(() => {
      reorderGuideRef.current?.remove();
      reorderGuideRef.current = null;

      reorderTooltipRef.current?.remove();
      reorderTooltipRef.current = null;

      removeReorderTooltips();
    }, []);

    React.useEffect(() => {
      return () => {
        cleanupReorderGuide();
      };
    }, [cleanupReorderGuide]);

    const applyResize = React.useCallback(
      (nextWidth: number) => {
        if (!enhancedColumnKey || !enhancedOnColumnResize) return;

        enhancedOnColumnResize(
          enhancedColumnKey,
          Math.max(enhancedMinColumnWidth, Math.round(nextWidth)),
        );
      },
      [enhancedColumnKey, enhancedMinColumnWidth, enhancedOnColumnResize],
    );

    const handleResizePointerDown = (
      event: React.PointerEvent<HTMLSpanElement>,
    ) => {
      if (
        !canUseDOM() ||
        !enhancedColumnKey ||
        !enhancedResizeEnabled ||
        !enhancedOnColumnResize
      ) {
        return;
      }

      event.preventDefault();
      event.stopPropagation();

      const th = event.currentTarget.closest("th") as HTMLElement | null;

      const startX = event.clientX;
      const startWidth =
        typeof enhancedWidth === "number"
          ? enhancedWidth
          : th?.getBoundingClientRect().width || enhancedMinColumnWidth;

      const guide = createResizeGuide();
      guide?.setX(event.clientX);

      let frame = 0;
      let latestWidth = startWidth;
      let latestClientX = event.clientX;
      let cleaned = false;

      setResizing(true);

      debugLog(enhancedDebug, "Resize start", {
        columnKey: enhancedColumnKey,
        startWidth,
      });

      const flush = () => {
        frame = 0;
        guide?.setX(latestClientX);
        applyResize(latestWidth);
      };

      const scheduleResize = (clientX: number, width: number) => {
        latestClientX = clientX;
        latestWidth = width;

        if (frame) return;
        frame = window.requestAnimationFrame(flush);
      };

      const handlePointerMove = (moveEvent: PointerEvent) => {
        const nextWidth = Math.max(
          enhancedMinColumnWidth,
          Math.round(startWidth + moveEvent.clientX - startX),
        );

        scheduleResize(moveEvent.clientX, nextWidth);
      };

      const cleanup = () => {
        if (cleaned) return;
        cleaned = true;

        if (frame) {
          window.cancelAnimationFrame(frame);
          frame = 0;
        }

        applyResize(latestWidth);
        guide?.remove();

        debugLog(enhancedDebug, "Resize end", {
          columnKey: enhancedColumnKey,
          width: latestWidth,
        });

        setResizing(false);

        document.removeEventListener("pointermove", handlePointerMove);
        document.removeEventListener("pointerup", cleanup);
        document.removeEventListener("pointercancel", cleanup);

        document.body.style.cursor = "";
        document.body.style.userSelect = "";
      };

      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";

      document.addEventListener("pointermove", handlePointerMove);
      document.addEventListener("pointerup", cleanup);
      document.addEventListener("pointercancel", cleanup);
    };

    const handleResizeKeyDown = (
      event: React.KeyboardEvent<HTMLSpanElement>,
    ) => {
      if (
        !enhancedColumnKey ||
        !enhancedResizeEnabled ||
        !enhancedOnColumnResize
      ) {
        return;
      }

      const currentWidth =
        typeof enhancedWidth === "number"
          ? enhancedWidth
          : enhancedMinColumnWidth;

      const step = event.shiftKey ? 25 : 10;

      if (event.key === "ArrowLeft") {
        event.preventDefault();
        event.stopPropagation();
        applyResize(currentWidth - step);
      }

      if (event.key === "ArrowRight") {
        event.preventDefault();
        event.stopPropagation();
        applyResize(currentWidth + step);
      }

      if (event.key === "Home") {
        event.preventDefault();
        event.stopPropagation();
        applyResize(enhancedMinColumnWidth);
      }
    };

    const handleDragOver = (event: React.DragEvent<HTMLTableCellElement>) => {
      onDragOver?.(event);

      if (
        !enhancedColumnKey ||
        !enhancedReorderEnabled ||
        !enhancedOnColumnDrop ||
        !controlsEnabled
      ) {
        cleanupReorderGuide();
        return;
      }

      const fromColumnKey =
        event.dataTransfer.getData(
          "application/x-antd-table-enhanced-column",
        ) ||
        event.dataTransfer.getData("text/plain") ||
        activeDragColumnKey;

      if (fromColumnKey && fromColumnKey === enhancedColumnKey) {
        setDragOverSide(null);
        cleanupReorderGuide();
        return;
      }

      event.preventDefault();

      const side = getDropSideFromEvent(event);
      const rect = event.currentTarget.getBoundingClientRect();
      const guideX = side === "left" ? rect.left : rect.right;

      setDragOverSide(side);

      if (!reorderGuideRef.current) {
        reorderGuideRef.current = createReorderGuide();
      }

      reorderGuideRef.current?.setX(guideX);

      const draggedColumnLabel = getDragColumnLabelFromEvent(
        event,
        fromColumnKey || undefined,
      );

      const targetColumnLabel =
        enhancedColumnLabel ||
        getColumnDisplayLabel(enhancedColumnKey, enhancedColumnTitle);

      const tooltipText = buildReorderTooltipText({
        draggedColumnKey: fromColumnKey,
        draggedColumnLabel,
        targetColumnKey: enhancedColumnKey,
        targetColumnLabel,
        previousColumnKey: enhancedPreviousColumnKey,
        previousColumnLabel: enhancedPreviousColumnLabel,
        side,
      });

      if (!reorderTooltipRef.current) {
        reorderTooltipRef.current = createReorderTooltip();
      }

      reorderTooltipRef.current?.setText(tooltipText);
      reorderTooltipRef.current?.setPosition(event.clientX, event.clientY);

      event.dataTransfer.dropEffect = "move";
    };

    const handleDragLeave = (event: React.DragEvent<HTMLTableCellElement>) => {
      onDragLeave?.(event);

      const nextTarget = event.relatedTarget as Node | null;

      if (nextTarget && event.currentTarget.contains(nextTarget)) {
        return;
      }

      setDragOverSide(null);
      cleanupReorderGuide();
    };

    const handleDragEnd = (event: React.DragEvent<HTMLTableCellElement>) => {
      onDragEnd?.(event);

      activeDragColumnKey = null;
      activeDragColumnLabel = null;

      setDragOverSide(null);
      cleanupReorderGuide();
      removeReorderGuides();
    };

    const handleDrop = (event: React.DragEvent<HTMLTableCellElement>) => {
      onDrop?.(event);

      if (
        !enhancedColumnKey ||
        !enhancedReorderEnabled ||
        !enhancedOnColumnDrop ||
        !controlsEnabled
      ) {
        setDragOverSide(null);
        cleanupReorderGuide();
        return;
      }

      event.preventDefault();
      event.stopPropagation();

      const fromColumnKey =
        event.dataTransfer.getData(
          "application/x-antd-table-enhanced-column",
        ) ||
        event.dataTransfer.getData("text/plain") ||
        activeDragColumnKey;

      const toColumnKey = enhancedColumnKey;
      const side = dragOverSide ?? getDropSideFromEvent(event);

      setDragOverSide(null);
      cleanupReorderGuide();
      removeReorderGuides();

      activeDragColumnKey = null;
      activeDragColumnLabel = null;

      debugLog(enhancedDebug, "Drop", {
        fromColumnKey,
        toColumnKey,
        side,
      });

      if (!fromColumnKey || fromColumnKey === toColumnKey) return;

      enhancedOnColumnDrop(fromColumnKey, toColumnKey, side);
    };

    const handleContextMenu = (
      event: React.MouseEvent<HTMLTableCellElement>,
    ) => {
      onContextMenu?.(event);

      if (
        event.defaultPrevented ||
        !enhancedColumnKey ||
        !enhancedOnContextMenuOpenChange
      ) {
        return;
      }

      event.preventDefault();
      event.stopPropagation();

      enhancedOnHeaderContextMenu?.({
        event,
        columnKey: enhancedColumnKey,
        columnTitle: enhancedColumnTitle,
      });

      enhancedOnContextMenuOpenChange(enhancedColumnKey, true);
    };

    const resizeHandle =
      controlsEnabled && enhancedResizeEnabled && enhancedColumnKey ? (
        <span
          className={cx(s.control, s.resizeHandle)}
          title="Drag to resize. Use Left/Right arrow keys for keyboard resize."
          aria-label={`Resize column ${enhancedColumnKey}`}
          role="separator"
          tabIndex={0}
          aria-orientation="vertical"
          onPointerDown={handleResizePointerDown}
          onKeyDown={handleResizeKeyDown}
          onClick={(event) => {
            event.preventDefault();
            event.stopPropagation();
          }}
          onContextMenu={(event) => {
            event.preventDefault();
            event.stopPropagation();
          }}
        />
      ) : null;

    const mergedClassName = cx(
      className,
      hovered && s.headerHover,
      resizing && s.headerResizing,
      dragOverSide === "left" && s.headerDragOverLeft,
      dragOverSide === "right" && s.headerDragOverRight,
      enhancedRecentlyMoved && s.headerRecentlyMoved,
    );

    const mergedStyle: React.CSSProperties = {
      ...style,
      width: enhancedWidth,
      minWidth: enhancedWidth,
      maxWidth: enhancedWidth,
    };

    const finalProps = {
      ...rest,
      className: mergedClassName,
      style: mergedStyle,
      onMouseEnter: (event: React.MouseEvent<HTMLTableCellElement>) => {
        setHovered(true);
        onMouseEnter?.(event);
      },
      onMouseLeave: (event: React.MouseEvent<HTMLTableCellElement>) => {
        setHovered(false);
        setDragOverSide(null);
        cleanupReorderGuide();
        onMouseLeave?.(event);
      },
      onDragOver: handleDragOver,
      onDragLeave: handleDragLeave,
      onDragEnd: handleDragEnd,
      onDrop: handleDrop,
      onContextMenu: handleContextMenu,
      "data-antd-table-enhanced-header-cell": "true",
      "data-antd-table-enhanced-column-key": enhancedColumnKey,
      "data-antd-table-enhanced-resize": enhancedResizeEnabled
        ? "true"
        : "false",
      "data-antd-table-enhanced-reorder": enhancedReorderEnabled
        ? "true"
        : "false",
      children: (
        <>
          {children}
          {resizeHandle}
        </>
      ),
    };

    const cell = ExistingHeaderCell ? (
      React.createElement(ExistingHeaderCell, finalProps)
    ) : (
      <th {...finalProps} />
    );

    if (
      !enhancedColumnKey ||
      !enhancedPreferenceMenuItems ||
      !enhancedOnPreferenceMenuClick ||
      !enhancedOnContextMenuOpenChange
    ) {
      return cell;
    }

    return (
      <Dropdown
        trigger={[]}
        placement="bottomLeft"
        open={Boolean(enhancedContextMenuOpen)}
        getPopupContainer={
          enhancedGetPopupContainer ?? getDefaultDropdownPopupContainer
        }
        overlayClassName={s.preferenceDropdown}
        overlayStyle={{
          width: "max-content",
          minWidth: 240,
          zIndex: DROPDOWN_OVERLAY_Z_INDEX,
        }}
        onOpenChange={(open) => {
          enhancedOnContextMenuOpenChange(enhancedColumnKey, open);
        }}
        menu={{
          selectable: false,
          items: enhancedPreferenceMenuItems,
          style: {
            width: "max-content",
            minWidth: 240,
          },
          onClick: ({ key, domEvent }) => {
            domEvent.stopPropagation();
            enhancedOnPreferenceMenuClick(enhancedColumnKey, String(key));
          },
        }}
      >
        {cell}
      </Dropdown>
    );
  }

  return EnhancedHeaderCell;
}

type ContextMenuState = {
  columnKey: string;
  columnTitle?: React.ReactNode;
} | null;

function decorateColumns<RecordType extends AnyRecord>(
  columns: TableEnhancedColumns<RecordType>,
  options: {
    widths: Record<string, number>;
    recentlyMovedKeys: string[];
    enableColumnResize: boolean;
    enableColumnReorder: boolean;
    minColumnWidth: number;
    defaultColumnWidth: number;
    showColumnControls: "always" | "hover" | "off";
    debug?: boolean;
    contextMenu: ContextMenuState;
    effectivePinned: Record<string, PinSide | undefined>;
    onContextMenuOpenChange: (columnKey: string, open: boolean) => void;
    onPreferenceMenuClick: (columnKey: string, actionKey: string) => void;
    getPreferenceMenuItems: (columnKey: string) => MenuProps["items"];
    getPopupContainer: (triggerNode: HTMLElement) => HTMLElement;
    onResize: (columnKey: string, width: number) => void;
    onDrop: (
      fromColumnKey: string,
      toColumnKey: string,
      side: DropSide,
    ) => void;
    onHeaderContextMenu: (payload: HeaderContextMenuPayload) => void;
    indexPath?: number[];
    topLevel?: boolean;
  },
): TableEnhancedColumns<RecordType> {
  const {
    widths,
    recentlyMovedKeys,
    enableColumnResize,
    enableColumnReorder,
    minColumnWidth,
    defaultColumnWidth,
    showColumnControls,
    debug,
    contextMenu,
    effectivePinned,
    onContextMenuOpenChange,
    onPreferenceMenuClick,
    getPreferenceMenuItems,
    getPopupContainer,
    onResize,
    onDrop,
    onHeaderContextMenu,
    indexPath = [],
    topLevel = true,
  } = options;

  return columns.map((column, index) => {
    const currentIndexPath = [...indexPath, index];
    const columnKey = getColumnKey(column, currentIndexPath);

    const previousColumn =
      topLevel && index > 0 ? columns[index - 1] : undefined;

    const previousColumnKey = previousColumn
      ? getColumnKey(previousColumn, [...indexPath, index - 1])
      : undefined;

    const previousColumnLabel = previousColumn
      ? getColumnDisplayLabel(
          previousColumnKey as string,
          typeof previousColumn.title === "function"
            ? undefined
            : previousColumn.title,
        )
      : undefined;

    const existingOnHeaderCell = column.onHeaderCell;
    const hasChildren =
      Array.isArray(column.children) && column.children.length > 0;

    const existingWidth = numericWidth(column.width);
    const persistedWidth = widths[columnKey];

    const finalWidth = hasChildren
      ? (persistedWidth ?? existingWidth)
      : (persistedWidth ?? existingWidth ?? defaultColumnWidth);

    const resizeEnabled =
      enableColumnResize && !hasChildren && isResizeEnabledForColumn(column);

    const reorderEnabled =
      enableColumnReorder && topLevel && isReorderEnabledForColumn(column);

    const originalTitle = column.title;

    const staticColumnLabel = getColumnDisplayLabel(
      columnKey,
      typeof originalTitle === "function" ? undefined : originalTitle,
    );

    const pinnedSide = effectivePinned[columnKey];

    const renderTitle = (titleProps?: any) => {
      const node =
        typeof originalTitle === "function"
          ? originalTitle(titleProps)
          : originalTitle;

      return (
        <EnhancedTitle
          columnKey={columnKey}
          columnLabel={getColumnDisplayLabel(columnKey, node)}
          reorderEnabled={reorderEnabled}
          controlsEnabled={showColumnControls !== "off"}
          pinnedSide={pinnedSide}
          debug={debug}
        >
          {node}
        </EnhancedTitle>
      );
    };

    const nextColumn: TableEnhancedColumn<RecordType> = {
      ...column,
      key: column.key ?? columnKey,
      width: finalWidth,
      title:
        typeof originalTitle === "function"
          ? (titleProps: any) => renderTitle(titleProps)
          : renderTitle(),
      onHeaderCell: function onHeaderCell(col) {
        const existingHeaderCellProps = existingOnHeaderCell?.(col) ?? {};

        return {
          ...existingHeaderCellProps,
          enhancedColumnKey: columnKey,
          enhancedColumnTitle:
            typeof originalTitle === "function" ? undefined : originalTitle,
          enhancedColumnLabel: staticColumnLabel,
          enhancedPreviousColumnKey: previousColumnKey,
          enhancedPreviousColumnLabel: previousColumnLabel,
          enhancedWidth: finalWidth,
          enhancedResizeEnabled: resizeEnabled,
          enhancedReorderEnabled: reorderEnabled,
          enhancedMinColumnWidth: minColumnWidth,
          enhancedShowColumnControls: showColumnControls,
          enhancedRecentlyMoved: recentlyMovedKeys.includes(columnKey),
          enhancedDebug: debug,

          enhancedContextMenuOpen: contextMenu?.columnKey === columnKey,
          enhancedPreferenceMenuItems: getPreferenceMenuItems(columnKey),
          enhancedGetPopupContainer: getPopupContainer,
          enhancedOnContextMenuOpenChange: onContextMenuOpenChange,
          enhancedOnPreferenceMenuClick: onPreferenceMenuClick,

          enhancedOnColumnResize: onResize,
          enhancedOnColumnDrop: onDrop,
          enhancedOnHeaderContextMenu: onHeaderContextMenu,
        };
      },
    };

    if (hasChildren) {
      nextColumn.children = decorateColumns(
        column.children as TableEnhancedColumns<RecordType>,
        {
          ...options,
          indexPath: currentIndexPath,
          topLevel: false,
        },
      );
    }

    return nextColumn;
  });
}

type PendingStorageWrite = {
  key: string;
  state: TableEnhancedState;
  storage?: Storage;
  debug?: boolean;
};

function getNestedValue(record: AnyRecord, dataIndex: unknown) {
  if (dataIndex === undefined || dataIndex === null) return undefined;

  const path = Array.isArray(dataIndex)
    ? dataIndex
    : String(dataIndex).split(".");

  return path.reduce((value, key) => {
    if (value === undefined || value === null) return undefined;
    return value[key as keyof typeof value];
  }, record as any);
}

function normalizeExportCellValue(value: any): string {
  if (value === undefined || value === null) return "";

  if (React.isValidElement(value)) {
    return reactNodeToPlainText(value) ?? "";
  }

  if (
    typeof value === "string" ||
    typeof value === "number" ||
    typeof value === "boolean"
  ) {
    return String(value);
  }

  if (value instanceof Date) {
    return value.toISOString();
  }

  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

type ExportColumn<RecordType> = {
  key: string;
  title: string;
  column: TableEnhancedColumn<RecordType>;
};

function collectExportColumns<RecordType extends AnyRecord>(
  columns: TableEnhancedColumns<RecordType>,
  indexPath: number[] = [],
): ExportColumn<RecordType>[] {
  return columns.flatMap((column, index) => {
    const currentIndexPath = [...indexPath, index];
    const columnKey = getColumnKey(column, currentIndexPath);

    if (Array.isArray(column.children) && column.children.length > 0) {
      return collectExportColumns(
        column.children as TableEnhancedColumns<RecordType>,
        currentIndexPath,
      );
    }

    const titleNode =
      typeof column.title === "function" ? undefined : column.title;

    return [
      {
        key: columnKey,
        title: getColumnDisplayLabel(columnKey, titleNode),
        column,
      },
    ];
  });
}

function getExportRows<RecordType extends AnyRecord>(
  dataSource: readonly RecordType[] | undefined,
  exportColumns: ExportColumn<RecordType>[],
) {
  const rows = Array.isArray(dataSource) ? dataSource : [];

  return rows.map((record, rowIndex) => {
    const row: Record<string, string> = {};

    exportColumns.forEach(({ title, column }) => {
      let rawValue = getNestedValue(record, column.dataIndex);

      if (typeof column.render === "function") {
        try {
          const rendered = column.render(rawValue, record, rowIndex) as any;

          rawValue =
            rendered && typeof rendered === "object" && "children" in rendered
              ? rendered.children
              : rendered;
        } catch {
          rawValue = getNestedValue(record, column.dataIndex);
        }
      }

      row[title] = normalizeExportCellValue(rawValue);
    });

    return row;
  });
}

function csvEscape(value: string) {
  const safe = value ?? "";
  if (/[",\n\r]/.test(safe)) {
    return `"${safe.replace(/"/g, '""')}"`;
  }
  return safe;
}

function downloadBlob(content: BlobPart, fileName: string, type: string) {
  if (!canUseDOM()) return;

  const blob = new Blob([content], { type });
  const url = URL.createObjectURL(blob);

  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = fileName;
  anchor.style.display = "none";

  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();

  window.setTimeout(() => URL.revokeObjectURL(url), 1200);
}

function exportToCsv<RecordType extends AnyRecord>(
  rows: Record<string, string>[],
  exportColumns: ExportColumn<RecordType>[],
  fileName: string,
) {
  const headers = exportColumns.map((column) => column.title);
  const content = [
    headers.map(csvEscape).join(","),
    ...rows.map((row) =>
      headers.map((header) => csvEscape(row[header] ?? "")).join(","),
    ),
  ].join("\n");

  downloadBlob(
    `\uFEFF${content}`,
    `${fileName}.csv`,
    "text/csv;charset=utf-8;",
  );
}

function htmlEscape(value: string) {
  return String(value ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;");
}

function exportToExcel<RecordType extends AnyRecord>(
  rows: Record<string, string>[],
  exportColumns: ExportColumn<RecordType>[],
  fileName: string,
) {
  const headers = exportColumns.map((column) => column.title);

  const tableHtml = `
    <html>
      <head>
        <meta charset="UTF-8" />
      </head>
      <body>
        <table>
          <thead>
            <tr>${headers.map((header) => `<th>${htmlEscape(header)}</th>`).join("")}</tr>
          </thead>
          <tbody>
            ${rows
              .map(
                (row) =>
                  `<tr>${headers
                    .map((header) => `<td>${htmlEscape(row[header] ?? "")}</td>`)
                    .join("")}</tr>`,
              )
              .join("")}
          </tbody>
        </table>
      </body>
    </html>
  `;

  downloadBlob(
    tableHtml,
    `${fileName}.xls`,
    "application/vnd.ms-excel;charset=utf-8;",
  );
}

function exportToJson(rows: Record<string, string>[], fileName: string) {
  downloadBlob(
    JSON.stringify(rows, null, 2),
    `${fileName}.json`,
    "application/json;charset=utf-8;",
  );
}

function getExportFileName(tableEnhancedKey?: string) {
  const date = new Date().toISOString().slice(0, 10);
  const safeKey = tableEnhancedKey
    ? tableEnhancedKey.replace(/[^\w.-]+/g, "_")
    : "table";

  return `${safeKey}_${date}`;
}

type ToolbarColumnItem = {
  key: string;
  label: string;
  pinned?: PinSide;
  visible: boolean;
};

function InnerTable<RecordType extends AnyRecord = AnyRecord>(
  props: TableEnhancedProps<RecordType>,
  ref: React.Ref<any>,
) {
  const {
    columns,
    components,
    tableEnhancedKey,
    tableEnhancedActionsRef,
    enableColumnResize = true,
    enableColumnReorder = true,
    allow_export = false,
    show_column_visibility = false,
    tableEnhancedDebug = false,
    tableEnhancedShowActiveBadge = false,
    minColumnWidth = 90,
    defaultColumnWidth = 180,
    showColumnControls = "hover",
    tableEnhancedDensity = "middle",
    tableEnhancedBorderedHeader = true,
    storage,
    rowKey,
    tableLayout,
    scroll,
    className,
    dataSource,
    onTableEnhancedColumnResize,
    onTableEnhancedColumnReorder,
    onTableEnhancedColumnPin,
    onTableEnhancedColumnVisibilityChange,
    ...restProps
  } = props;

  const debug = isDebugEnabled(tableEnhancedDebug);

  useIsomorphicLayoutEffect(() => {
    // Styles are supplied via Table.module.css.
  }, []);

  const storageKey = React.useMemo(() => {
    return tableEnhancedKey
      ? `${STORAGE_PREFIX}:${tableEnhancedKey}`
      : buildAutoStorageKey(columns, rowKey);
  }, [tableEnhancedKey, columns, rowKey]);

  const [persisted, setPersisted] = React.useState<TableEnhancedState>(() =>
    safeReadStorage(storageKey, storage, debug),
  );

  const persistedRef = React.useRef(persisted);
  const pendingStorageWriteRef = React.useRef<PendingStorageWrite | null>(null);
  const storageWriteTimerRef = React.useRef<number | null>(null);

  const [recentlyMovedKeys, setRecentlyMovedKeys] = React.useState<string[]>(
    [],
  );

  const [contextMenu, setContextMenu] = React.useState<ContextMenuState>(null);
  const [columnVisibilityOpen, setColumnVisibilityOpen] = React.useState(false);
  const [columnSearch, setColumnSearch] = React.useState("");
  const [exportOpen, setExportOpen] = React.useState(false);
  const [exportingType, setExportingType] = React.useState<
    "csv" | "excel" | "json" | null
  >(null);

  const searchInputRef = React.useRef<any>(null);
  const wrapperRef = React.useRef<HTMLDivElement | null>(null);

  React.useEffect(() => {
    persistedRef.current = persisted;
  }, [persisted]);

  React.useEffect(() => {
    if (!columnVisibilityOpen) return;

    const timer = window.setTimeout(() => {
      searchInputRef.current?.focus?.();
      searchInputRef.current?.select?.();
    }, 80);

    return () => window.clearTimeout(timer);
  }, [columnVisibilityOpen]);

  const flushPendingStorageWrite = React.useCallback(() => {
    if (storageWriteTimerRef.current && canUseDOM()) {
      window.clearTimeout(storageWriteTimerRef.current);
      storageWriteTimerRef.current = null;
    }

    const pending = pendingStorageWriteRef.current;
    pendingStorageWriteRef.current = null;

    if (!pending) return;

    if (hasMeaningfulState(pending.state)) {
      safeWriteStorage(
        pending.key,
        pending.state,
        pending.storage,
        pending.debug,
      );
    } else {
      safeRemoveStorage(pending.key, pending.storage, pending.debug);
    }
  }, []);

  const scheduleStorageWrite = React.useCallback(
    (state: TableEnhancedState, immediate = false) => {
      pendingStorageWriteRef.current = {
        key: storageKey,
        state,
        storage,
        debug,
      };

      if (!canUseDOM() || immediate) {
        flushPendingStorageWrite();
        return;
      }

      if (storageWriteTimerRef.current) {
        window.clearTimeout(storageWriteTimerRef.current);
      }

      storageWriteTimerRef.current = window.setTimeout(() => {
        flushPendingStorageWrite();
      }, STORAGE_WRITE_DEBOUNCE_MS);
    },
    [storageKey, storage, debug, flushPendingStorageWrite],
  );

  React.useEffect(() => {
    return () => {
      flushPendingStorageWrite();
    };
  }, [flushPendingStorageWrite]);

  const originalColumns = React.useMemo(
    () => (columns ?? []) as TableEnhancedColumns<RecordType>,
    [columns],
  );

  const originalNormalizedColumns = React.useMemo(
    () => normalizeFixedColumnPlacement(originalColumns),
    [originalColumns],
  );

  const topLevelColumnKeys = React.useMemo(
    () => getTopLevelColumnKeys(originalNormalizedColumns),
    [originalNormalizedColumns],
  );

  const allColumnKeys = React.useMemo(
    () => getAllColumnKeys(originalNormalizedColumns),
    [originalNormalizedColumns],
  );

  React.useEffect(() => {
    flushPendingStorageWrite();

    const loaded = safeReadStorage(storageKey, storage, debug);
    const sanitized = sanitizePersistedState(
      loaded,
      allColumnKeys,
      topLevelColumnKeys,
    );

    persistedRef.current = sanitized;
    setPersisted(sanitized);
    setRecentlyMovedKeys([]);
    setContextMenu(null);
    setColumnVisibilityOpen(false);
    setExportOpen(false);
  }, [
    storageKey,
    storage,
    debug,
    allColumnKeys,
    topLevelColumnKeys,
    flushPendingStorageWrite,
  ]);

  React.useEffect(() => {
    setPersisted((current) => {
      const sanitized = sanitizePersistedState(
        current,
        allColumnKeys,
        topLevelColumnKeys,
      );

      if (statesEqual(current, sanitized)) return current;

      persistedRef.current = sanitized;
      scheduleStorageWrite(sanitized);
      return sanitized;
    });
  }, [allColumnKeys, topLevelColumnKeys, scheduleStorageWrite]);

  const updatePersisted = React.useCallback(
    (
      updater: (current: TableEnhancedState) => TableEnhancedState,
      options?: { immediateStorageWrite?: boolean },
    ) => {
      setPersisted((current) => {
        const nextRaw = updater(current);
        const next = sanitizePersistedState(
          nextRaw,
          allColumnKeys,
          topLevelColumnKeys,
        );

        if (statesEqual(current, next)) return current;

        persistedRef.current = next;
        scheduleStorageWrite(next, options?.immediateStorageWrite);
        return next;
      });
    },
    [allColumnKeys, topLevelColumnKeys, scheduleStorageWrite],
  );

  const pinnedAppliedColumns = React.useMemo(() => {
    return applyPinnedColumns(
      originalNormalizedColumns,
      persisted.pinned ?? {},
    );
  }, [originalNormalizedColumns, persisted.pinned]);

  const fixedNormalizedColumns = React.useMemo(() => {
    return normalizeFixedColumnPlacement(pinnedAppliedColumns);
  }, [pinnedAppliedColumns]);

  const orderedColumns = React.useMemo(() => {
    return orderColumns(fixedNormalizedColumns, persisted.order, debug);
  }, [fixedNormalizedColumns, persisted.order, debug]);

  const visibleOrderedColumns = React.useMemo(() => {
    return filterHiddenColumns(orderedColumns, persisted.hidden ?? []);
  }, [orderedColumns, persisted.hidden]);

  const effectivePinned = React.useMemo(() => {
    const result: Record<string, PinSide | undefined> = {};

    orderedColumns.forEach((column, index) => {
      const key = getColumnKey(column, [index]);

      if (column.fixed === true || column.fixed === "left") {
        result[key] = "left";
      } else if (column.fixed === "right") {
        result[key] = "right";
      }
    });

    return result;
  }, [orderedColumns]);

  const hasCustomOrder = persisted.order.length > 0;
  const hasAnyPreference = hasMeaningfulState(persisted);

  const resetLayout = React.useCallback(() => {
    if (!hasMeaningfulState(persistedRef.current)) {
      setContextMenu(null);
      return;
    }

    const next = emptyState();

    persistedRef.current = next;
    setPersisted(next);
    scheduleStorageWrite(next, true);

    setRecentlyMovedKeys([]);
    setContextMenu(null);

    debugLog(debug, "Layout reset", { storageKey });
  }, [scheduleStorageWrite, debug, storageKey]);

  const resetColumnWidth = React.useCallback(
    (columnKey: string) => {
      if (
        !Object.prototype.hasOwnProperty.call(
          persistedRef.current.widths,
          columnKey,
        )
      ) {
        setContextMenu(null);
        return;
      }

      updatePersisted((current) => {
        const nextWidths = { ...current.widths };
        delete nextWidths[columnKey];

        return {
          ...current,
          widths: nextWidths,
        };
      });

      setContextMenu(null);

      debugLog(debug, "Column width reset", { columnKey });
    },
    [updatePersisted, debug],
  );

  const resetColumnOrder = React.useCallback(() => {
    if (persistedRef.current.order.length === 0) {
      setContextMenu(null);
      return;
    }

    updatePersisted((current) => ({
      ...current,
      order: [],
    }));

    setRecentlyMovedKeys([]);
    setContextMenu(null);
    onTableEnhancedColumnReorder?.([]);

    debugLog(debug, "Column order reset");
  }, [updatePersisted, debug, onTableEnhancedColumnReorder]);

  const pinColumn = React.useCallback(
    (columnKey: string, side: PinSide) => {
      updatePersisted((current) => ({
        ...current,
        pinned: {
          ...(current.pinned ?? {}),
          [columnKey]: side,
        },
      }));

      setRecentlyMovedKeys([columnKey]);
      setContextMenu(null);
      onTableEnhancedColumnPin?.(columnKey, side);

      if (canUseDOM()) {
        window.setTimeout(() => setRecentlyMovedKeys([]), 650);
      }
    },
    [updatePersisted, onTableEnhancedColumnPin],
  );

  const unpinColumn = React.useCallback(
    (columnKey: string) => {
      updatePersisted((current) => ({
        ...current,
        pinned: {
          ...(current.pinned ?? {}),
          [columnKey]: null,
        },
      }));

      setRecentlyMovedKeys([columnKey]);
      setContextMenu(null);
      onTableEnhancedColumnPin?.(columnKey, null);

      if (canUseDOM()) {
        window.setTimeout(() => setRecentlyMovedKeys([]), 650);
      }
    },
    [updatePersisted, onTableEnhancedColumnPin],
  );

  const setColumnVisible = React.useCallback(
    (columnKey: string, visible: boolean) => {
      updatePersisted((current) => {
        const hiddenSet = new Set(current.hidden ?? []);

        if (visible) {
          hiddenSet.delete(columnKey);
        } else {
          hiddenSet.add(columnKey);
        }

        return {
          ...current,
          hidden: Array.from(hiddenSet),
        };
      });

      onTableEnhancedColumnVisibilityChange?.(columnKey, visible);
    },
    [updatePersisted, onTableEnhancedColumnVisibilityChange],
  );

  const applyAutoFitTargets = React.useCallback(
    (
      targets: AutoFitColumnTarget<RecordType>[],
      options?: {
        successMessage?: string;
      },
    ) => {
      const uniqueTargets = Array.from(
        new Map(targets.map((target) => [target.key, target])).values(),
      );

      if (!uniqueTargets.length) {
        message.warning("No visible columns available to autofit.");
        setContextMenu(null);
        return;
      }

      const nextWidths: Record<string, number> = {};

      uniqueTargets.forEach((target) => {
        nextWidths[target.key] = calculateAutoFitColumnWidth({
          target,
          dataSource: dataSource as readonly RecordType[] | undefined,
          wrapperElement: wrapperRef.current,
          minColumnWidth,
        });
      });

      updatePersisted((current) => ({
        ...current,
        widths: {
          ...current.widths,
          ...nextWidths,
        },
      }));

      Object.entries(nextWidths).forEach(([columnKey, width]) => {
        onTableEnhancedColumnResize?.(columnKey, width);
      });

      setContextMenu(null);

      message.success(
        options?.successMessage ??
          `Autofitted ${uniqueTargets.length} column${
            uniqueTargets.length === 1 ? "" : "s"
          }.`,
      );
    },
    [dataSource, minColumnWidth, updatePersisted, onTableEnhancedColumnResize],
  );

  const autoFitColumn = React.useCallback(
    (columnKey: string) => {
      const targets = findAutoFitTargetsForColumnKey(
        visibleOrderedColumns,
        columnKey,
      );

      applyAutoFitTargets(targets, {
        successMessage: "Column autofitted.",
      });
    },
    [visibleOrderedColumns, applyAutoFitTargets],
  );

  const autoFitTable = React.useCallback(() => {
    const targets = collectAutoFitLeafTargets(visibleOrderedColumns);

    applyAutoFitTargets(targets, {
      successMessage: "Table autofitted.",
    });
  }, [visibleOrderedColumns, applyAutoFitTargets]);

  React.useEffect(() => {
    if (!tableEnhancedActionsRef) return;

    tableEnhancedActionsRef.current = {
      resetLayout,
      resetColumnWidth,
      resetColumnOrder,
      pinColumn,
      unpinColumn,
      setColumnVisible,
      autoFitColumn,
      autoFitTable,
      getState: () => persistedRef.current,
      setState: (state: TableEnhancedState) => {
        const next = sanitizePersistedState(
          state,
          allColumnKeys,
          topLevelColumnKeys,
        );

        persistedRef.current = next;
        setPersisted(next);
        scheduleStorageWrite(next, true);
      },
    };

    return () => {
      tableEnhancedActionsRef.current = null;
    };
  }, [
    tableEnhancedActionsRef,
    resetLayout,
    resetColumnWidth,
    resetColumnOrder,
    pinColumn,
    unpinColumn,
    setColumnVisible,
    autoFitColumn,
    autoFitTable,
    allColumnKeys,
    topLevelColumnKeys,
    scheduleStorageWrite,
  ]);

  const handleResize = React.useCallback(
    (columnKey: string, width: number) => {
      const safeWidth = Math.max(minColumnWidth, Math.round(width));

      if (persistedRef.current.widths[columnKey] === safeWidth) return;

      updatePersisted((current) => ({
        ...current,
        widths: {
          ...current.widths,
          [columnKey]: safeWidth,
        },
      }));

      onTableEnhancedColumnResize?.(columnKey, safeWidth);
    },
    [updatePersisted, minColumnWidth, onTableEnhancedColumnResize],
  );

  const handleDrop = React.useCallback(
    (fromColumnKey: string, toColumnKey: string, side: DropSide) => {
      const allBaseKeys = getTopLevelColumnKeys(fixedNormalizedColumns);

      const reorderableBaseKeys = fixedNormalizedColumns
        .map((column, index) => ({
          key: getColumnKey(column, [index]),
          column,
        }))
        .filter(({ column }) => isReorderEnabledForColumn(column))
        .map(({ key }) => key);

      if (
        !reorderableBaseKeys.includes(fromColumnKey) ||
        !reorderableBaseKeys.includes(toColumnKey)
      ) {
        debugWarn(debug, "Reorder ignored", {
          fromColumnKey,
          toColumnKey,
          side,
        });
        return;
      }

      setRecentlyMovedKeys([fromColumnKey, toColumnKey]);

      if (canUseDOM()) {
        window.setTimeout(() => {
          setRecentlyMovedKeys([]);
        }, 650);
      }

      updatePersisted((current) => {
        const currentOrder =
          current.order.length > 0
            ? normalizePersistedOrder(current.order, allBaseKeys)
            : allBaseKeys;

        const fromIndex = currentOrder.indexOf(fromColumnKey);
        const toIndex = currentOrder.indexOf(toColumnKey);

        if (fromIndex === -1 || toIndex === -1 || fromIndex === toIndex) {
          return current;
        }

        const nextOrder = moveArrayItemByDropSide(
          currentOrder,
          fromIndex,
          toIndex,
          side,
        );

        debugLog(debug, "Reorder applied", {
          side,
          before: currentOrder,
          after: nextOrder,
        });

        onTableEnhancedColumnReorder?.(nextOrder);

        return {
          ...current,
          order: nextOrder,
        };
      });
    },
    [
      fixedNormalizedColumns,
      updatePersisted,
      debug,
      onTableEnhancedColumnReorder,
    ],
  );

  const handleHeaderContextMenu = React.useCallback(
    ({ columnKey, columnTitle }: HeaderContextMenuPayload) => {
      setContextMenu({
        columnKey,
        columnTitle,
      });

      debugLog(debug, "Header context menu", {
        columnKey,
      });
    },
    [debug],
  );

  const handleContextMenuOpenChange = React.useCallback(
    (columnKey: string, open: boolean) => {
      if (open) {
        setContextMenu((current) => ({
          columnKey,
          columnTitle:
            current?.columnKey === columnKey ? current.columnTitle : undefined,
        }));
        return;
      }

      setContextMenu((current) => {
        if (current?.columnKey === columnKey) return null;
        return current;
      });
    },
    [],
  );

  React.useEffect(() => {
    if (!contextMenu || !canUseDOM()) return;

    const close = () => setContextMenu(null);

    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") close();
    };

    window.addEventListener("click", close);
    window.addEventListener("resize", close);
    window.addEventListener("scroll", close, true);
    window.addEventListener("keydown", handleKeyDown);

    return () => {
      window.removeEventListener("click", close);
      window.removeEventListener("resize", close);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("keydown", handleKeyDown);
    };
  }, [contextMenu]);

  const getPreferenceMenuItems = React.useCallback(
    (columnKey: string): MenuProps["items"] => {
      const columnHasSavedWidth = Object.prototype.hasOwnProperty.call(
        persisted.widths,
        columnKey,
      );

      const pinnedSide = effectivePinned[columnKey];

      const pinItems: MenuProps["items"] = pinnedSide
        ? [
            {
              key: "unpin-column",
              icon: <PinIcon />,
              label: "Unpin Column",
            },
          ]
        : [
            {
              key: "pin-left",
              icon: <PinIcon rotate={-45} />,
              label: "Pin to left",
            },
            {
              key: "pin-right",
              icon: <PinIcon rotate={45} />,
              label: "Pin to right",
            },
          ];

      return [
        ...pinItems,
        {
          type: "divider",
        },
        {
          key: "autofit-column",
          icon: <ColumnWidthIcon />,
          label: "Autofit Column",
        },
        {
          key: "autofit-table",
          icon: <ColumnWidthIcon />,
          label: "Autofit Table",
        },
        {
          type: "divider",
        },
        {
          key: "reset-width",
          icon: <ColumnWidthIcon />,
          label: "Reset width",
          disabled: !columnHasSavedWidth,
        },
        {
          key: "reset-order",
          icon: <MoveIcon />,
          label: "Reset order",
          disabled: !hasCustomOrder,
        },
        {
          type: "divider",
        },
        {
          key: "reset-all",
          icon: <ResetIcon />,
          label: "Reset preferences",
          disabled: !hasAnyPreference,
          danger: true,
        },
      ];
    },
    [persisted.widths, effectivePinned, hasCustomOrder, hasAnyPreference],
  );

  const handlePreferenceMenuClick = React.useCallback(
    (columnKey: string, actionKey: string) => {
      if (actionKey === "pin-left") {
        pinColumn(columnKey, "left");
        return;
      }

      if (actionKey === "pin-right") {
        pinColumn(columnKey, "right");
        return;
      }

      if (actionKey === "unpin-column") {
        unpinColumn(columnKey);
        return;
      }
      if (actionKey === "autofit-column") {
        autoFitColumn(columnKey);
        return;
      }

      if (actionKey === "autofit-table") {
        autoFitTable();
        return;
      }

      if (actionKey === "reset-width") {
        const columnHasSavedWidth = Object.prototype.hasOwnProperty.call(
          persistedRef.current.widths,
          columnKey,
        );

        if (!columnHasSavedWidth) return;

        resetColumnWidth(columnKey);
        return;
      }

      if (actionKey === "reset-order") {
        if (persistedRef.current.order.length === 0) return;

        resetColumnOrder();
        return;
      }

      if (actionKey === "reset-all") {
        if (!hasMeaningfulState(persistedRef.current)) return;

        resetLayout();
      }
    },
    [
      pinColumn,
      unpinColumn,
      autoFitColumn,
      autoFitTable,
      resetColumnWidth,
      resetColumnOrder,
      resetLayout,
    ],
  );

  const getDropdownPopupContainer = React.useCallback(
    (triggerNode: HTMLElement) => {
      if (!canUseDOM()) return triggerNode;
      // Always use body to avoid stacking-context / overflow:hidden traps.
      return document.body;
    },
    [],
  );

  const finalColumns = React.useMemo(() => {
    if (!columns) return columns;

    // The handlers below close over refs, and the lint rule cannot see that
    // none of them RUNS during render: they are handed to the header cells and
    // called from a pointer event, a drag or a menu click. Reading a ref there
    // is exactly what refs are for.
    // eslint-disable-next-line react-hooks/refs
    return decorateColumns(visibleOrderedColumns, {
      widths: persisted.widths,
      recentlyMovedKeys,
      enableColumnResize,
      enableColumnReorder,
      minColumnWidth,
      defaultColumnWidth,
      showColumnControls,
      debug,
      contextMenu,
      effectivePinned,
      onContextMenuOpenChange: handleContextMenuOpenChange,
      onPreferenceMenuClick: handlePreferenceMenuClick,
      getPreferenceMenuItems,
      getPopupContainer: getDropdownPopupContainer,
      onResize: handleResize,
      onDrop: handleDrop,
      onHeaderContextMenu: handleHeaderContextMenu,
    }) as ColumnsType<RecordType>;
  }, [
    columns,
    visibleOrderedColumns,
    persisted.widths,
    recentlyMovedKeys,
    enableColumnResize,
    enableColumnReorder,
    minColumnWidth,
    defaultColumnWidth,
    showColumnControls,
    debug,
    contextMenu,
    effectivePinned,
    handleContextMenuOpenChange,
    handlePreferenceMenuClick,
    getPreferenceMenuItems,
    getDropdownPopupContainer,
    handleResize,
    handleDrop,
    handleHeaderContextMenu,
  ]);

  const mergedComponents = React.useMemo(() => {
    const ExistingHeaderCell = components?.header?.cell;

    return {
      ...components,
      header: {
        ...components?.header,
        cell: createHeaderCell(ExistingHeaderCell),
      },
    };
  }, [components]);

  const finalScroll = React.useMemo(() => {
    if (scroll) {
      return {
        ...scroll,
        x: scroll.x ?? "max-content",
      };
    }

    return {
      x: "max-content",
    };
  }, [scroll]);

  const toolbarColumns = React.useMemo<ToolbarColumnItem[]>(() => {
    const hiddenSet = new Set(persisted.hidden ?? []);

    return orderedColumns.map((column, index) => {
      const key = getColumnKey(column, [index]);
      const titleNode =
        typeof column.title === "function" ? undefined : column.title;

      return {
        key,
        label: getColumnDisplayLabel(key, titleNode),
        pinned: effectivePinned[key],
        visible: !hiddenSet.has(key),
      };
    });
  }, [orderedColumns, persisted.hidden, effectivePinned]);

  const visibleCount = toolbarColumns.filter((column) => column.visible).length;

  const filteredToolbarColumns = React.useMemo(() => {
    const q = columnSearch.trim().toLowerCase();

    if (!q) return toolbarColumns;

    return toolbarColumns.filter((column) =>
      column.label.toLowerCase().includes(q),
    );
  }, [toolbarColumns, columnSearch]);

  const exportColumns = React.useMemo(() => {
    return collectExportColumns(visibleOrderedColumns);
  }, [visibleOrderedColumns]);

  const handleExport = React.useCallback(
    async (type: "csv" | "excel" | "json") => {
      if (!allow_export) return;

      setExportingType(type);

      try {
        await new Promise((resolve) => window.setTimeout(resolve, 120));

        const rows = getExportRows(
          dataSource as readonly RecordType[],
          exportColumns,
        );
        const fileName = getExportFileName(tableEnhancedKey);

        if (!exportColumns.length) {
          message.warning("No visible columns available to export.");
          return;
        }

        if (type === "csv") {
          exportToCsv(rows, exportColumns, fileName);
        }

        if (type === "excel") {
          exportToExcel(rows, exportColumns, fileName);
        }

        if (type === "json") {
          exportToJson(rows, fileName);
        }

        message.success("Export completed.");
        setExportOpen(false);
      } catch (error) {
        debugWarn(debug, "Export failed", error);
        message.error("Export failed. Please try again.");
      } finally {
        setExportingType(null);
      }
    },
    [allow_export, dataSource, exportColumns, tableEnhancedKey, debug],
  );

  const exportMenuItems: MenuProps["items"] = [
    {
      key: "csv",
      icon: <DocumentIcon />,
      label: "CSV",
      disabled: Boolean(exportingType),
    },
    {
      key: "excel",
      icon: <SheetIcon />,
      label: "Excel",
      disabled: Boolean(exportingType),
    },
    {
      key: "json",
      icon: <DocumentIcon />,
      label: "JSON",
      disabled: Boolean(exportingType),
    },
  ];

  const columnVisibilityDropdown = (
    <div
      className={s.columnVisibilityDropdown}
      onClick={(event) => event.stopPropagation()}
    >
      <div className={s.columnVisibilityHeader}>
        <Typography.Text strong>Columns</Typography.Text>
        <Typography.Text type="secondary" className={s.columnVisibilityCount}>
          {visibleCount}/{toolbarColumns.length} visible
        </Typography.Text>
      </div>

      <Input
        ref={searchInputRef}
        allowClear
        size="middle"
        prefix={<SearchIcon />}
        placeholder="Search columns"
        value={columnSearch}
        onChange={(event) => setColumnSearch(event.target.value)}
        className={s.columnSearch}
      />

      <div className={s.columnVisibilityList}>
        {filteredToolbarColumns.length ? (
          filteredToolbarColumns.map((column) => {
            const disableUncheck = column.visible && visibleCount <= 1;

            return (
              <label key={column.key} className={s.columnVisibilityItem}>
                <Checkbox
                  checked={column.visible}
                  disabled={disableUncheck}
                  onChange={(event) =>
                    setColumnVisible(column.key, event.target.checked)
                  }
                />

                <span className={s.columnVisibilityLabel}>{column.label}</span>

                {column.pinned ? (
                  <Tooltip title={`Pinned to ${column.pinned}`}>
                    <PinFilledIcon className={s.columnVisibilityPin} />
                  </Tooltip>
                ) : null}
              </label>
            );
          })
        ) : (
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description="No columns found"
            className={s.columnVisibilityEmpty}
          />
        )}
      </div>

      <div className={s.columnVisibilityFooter}>
        <Button
          size="small"
          type="link"
          disabled={!persisted.hidden?.length}
          onClick={() => {
            updatePersisted((current) => ({
              ...current,
              hidden: [],
            }));
          }}
        >
          Show all
        </Button>

        <Button
          size="small"
          onClick={() => {
            setColumnSearch("");
            setColumnVisibilityOpen(false);
          }}
        >
          Done
        </Button>
      </div>
    </div>
  );

  const wrapperClassName = cx(
    s.wrapper,
    tableEnhancedDensity === "compact" && s.densityCompact,
    tableEnhancedDensity === "middle" && s.densityMiddle,
    tableEnhancedDensity === "comfortable" && s.densityComfortable,
    showColumnControls === "always" && s.controlsAlways,
    tableEnhancedBorderedHeader && s.borderedHeader,
  );

  const showToolbar = allow_export || show_column_visibility;

  return (
    <div
      ref={wrapperRef}
      className={wrapperClassName}
      data-antd-table-enhanced-wrapper="true"
      data-antd-table-enhanced-key={tableEnhancedKey || storageKey}
    >
      {tableEnhancedShowActiveBadge ? (
        <div className={s.activeBadge}>
          <Tag color="blue" icon={<MoveIcon />}>
            AntD Table Enhanced Active
          </Tag>

          <Tag color="geekblue">Columns: {originalColumns.length}</Tag>

          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            Key: {tableEnhancedKey || storageKey}
          </Typography.Text>

          <Button
            size="small"
            icon={<ResetIcon />}
            onClick={resetLayout}
            disabled={!hasAnyPreference}
          >
            Reset layout
          </Button>
        </div>
      ) : null}

      {showToolbar ? (
        <div className={s.toolbar}>
          <Space size={8}>
            {allow_export ? (
              <Dropdown
                trigger={["click"]}
                open={exportOpen}
                onOpenChange={setExportOpen}
                overlayClassName={s.toolbarDropdown}
                overlayStyle={{
                  zIndex: DROPDOWN_OVERLAY_Z_INDEX,
                }}
                menu={{
                  items: exportMenuItems,
                  onClick: ({ key }) => {
                    handleExport(String(key) as "csv" | "excel" | "json");
                  },
                }}
              >
                <Tooltip title="Export table">
                  <Button
                    shape="circle"
                    icon={<ExportIcon />}
                    loading={Boolean(exportingType)}
                    aria-label="Export table"
                  />
                </Tooltip>
              </Dropdown>
            ) : null}

            {show_column_visibility ? (
              <Dropdown
                trigger={["click"]}
                open={columnVisibilityOpen}
                onOpenChange={(open) => {
                  setColumnVisibilityOpen(open);
                  if (open) setColumnSearch("");
                }}
                overlayClassName={s.columnVisibilityOverlay}
                overlayStyle={{
                  zIndex: DROPDOWN_OVERLAY_Z_INDEX,
                }}
                dropdownRender={() => columnVisibilityDropdown}
              >
                <Tooltip title="Column visibility">
                  <Button
                    shape="circle"
                    icon={<VisibilityIcon />}
                    aria-label="Column visibility"
                  />
                </Tooltip>
              </Dropdown>
            ) : null}
          </Space>
        </div>
      ) : null}

      <AntTable<RecordType>
        ref={ref}
        {...restProps}
        className={className}
        dataSource={dataSource}
        rowKey={rowKey}
        columns={finalColumns}
        components={mergedComponents}
        tableLayout={tableLayout ?? "fixed"}
        scroll={finalScroll}
      />
    </div>
  );
}

export type TableEnhancedComponent = (<
  RecordType extends AnyRecord = AnyRecord,
>(
  props: TableEnhancedProps<RecordType> & React.RefAttributes<any>,
) => React.ReactElement) &
  Pick<
    typeof AntTable,
    "Column" | "ColumnGroup" | "Summary" | "SELECTION_COLUMN" | "EXPAND_COLUMN"
  > & {
    displayName?: string;
  };

const ForwardedTable = React.forwardRef(InnerTable) as unknown as <
  RecordType extends AnyRecord = AnyRecord,
>(
  props: TableEnhancedProps<RecordType> & React.RefAttributes<any>,
) => React.ReactElement;

const Table = ForwardedTable as TableEnhancedComponent;

Table.Column = AntTable.Column;
Table.ColumnGroup = AntTable.ColumnGroup;
Table.Summary = AntTable.Summary;
Table.SELECTION_COLUMN = AntTable.SELECTION_COLUMN;
Table.EXPAND_COLUMN = AntTable.EXPAND_COLUMN;
Table.displayName = "Table";

export { Table };
