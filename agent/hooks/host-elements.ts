// The 2.1.311 runtime allow-list, also verified on 2.1.323.
// key and hover are handled separately by the host. Keep the floor on newer runtimes.
export const hostProps = {
  "Box": [
    "key",
    "hover",
    "position",
    "top",
    "left",
    "right",
    "bottom",
    "flexDirection",
    "flexGrow",
    "flexShrink",
    "flexWrap",
    "alignItems",
    "alignSelf",
    "justifyContent",
    "gap",
    "columnGap",
    "rowGap",
    "width",
    "height",
    "minWidth",
    "minHeight",
    "margin",
    "marginX",
    "marginY",
    "marginTop",
    "marginBottom",
    "marginLeft",
    "marginRight",
    "padding",
    "paddingX",
    "paddingY",
    "paddingTop",
    "paddingBottom",
    "paddingLeft",
    "paddingRight",
    "borderStyle",
    "borderColor",
    "borderDimColor",
    "backgroundColor",
    "overflow",
    "display"
  ],
  "Text": [
    "hover",
    "color",
    "backgroundColor",
    "dimColor",
    "size",
    "weight",
    "mono",
    "tabular",
    "bold",
    "italic",
    "underline",
    "strikethrough",
    "inverse",
    "dir",
    "wrap"
  ]
} as const

// Floor enum values and the plugin's accepted style palette. Other values are
// checked by the host harness; tests also run on the floor runtime itself.
export const hostValues = {
  Box: {
    flexDirection: ['row', 'column', 'row-reverse', 'column-reverse'],
    alignSelf: ['flex-start', 'center', 'flex-end', 'auto'],
    borderStyle: ['round'],
    borderColor: ['gray'],
  },
  Text: {
    wrap: ['wrap', 'end', 'middle', 'truncate-end', 'truncate', 'truncate-middle', 'truncate-start'],
    color: ['green', 'red', 'gray', 'yellow'],
    dimColor: [true, false],
  },
} as const
