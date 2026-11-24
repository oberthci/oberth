"use strict";
window.OberthStatus = {
 settings(h, status) {
  return h("section", {class: "effective-settings", "aria-label": "Effective settings"},
   h("h2", {}, "Effective settings"),
   ...(status.parts || []).filter(part => part.remedy).map(part => h("p", {}, part.display_reason + ". " + part.remedy)),
   h("div", {class: "settings-scroll", tabindex: "0", role: "region", "aria-label": "Settings table"},
    h("div", {class: "settings-table"}, ...(status.settings || []).map(setting =>
     h("div", {class: "setting-row"}, h("span", {}, setting.name), h("span", {class: "muted"}, setting.source), h("span", {class: "mono"}, setting.value))))));
 }
};
