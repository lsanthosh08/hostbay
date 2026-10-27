// One stacking scale (--layer-* in styles.css), lowest first: popovers,
// dialogs, menus, then toasts above everything so errors stay visible.
export const layer = {
  popover: "z-(--layer-popover)",
  dialog: "z-(--layer-dialog)",
  menu: "z-(--layer-menu)",
  toast: "z-(--layer-toast)",
} as const;
