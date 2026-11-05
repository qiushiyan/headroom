// Claude Code 2.1.318's store-dir naming, copied verbatim from its binary
// (helpers Cbe, Pe, k, U0). `node store-names.js > store-names.jsonl`
// regenerates the vectors TestMungeIsTheVendorsStoreDirName reads.
function Cbe(t){let e=0;for(let n=0;n<t.length;n++)e=(e<<5)-e+t.charCodeAt(n)|0;return e}
var cRe=200;function Pe(e){return Math.abs(Cbe(e)).toString(36)}function k(e){return e.replace(/[^a-zA-Z0-9]/g,"-")}function U0(e){let n=k(e);if(n.length<=cRe)return n;return`${n.slice(0,cRe)}-${Pe(e)}`}
const inputs = [
  "/Users/q/dev/.worktrees/x",
  "/a/my_project (v2)",
  "/a/café",
  "/a/\u{1F642}",
  "/" + "d".repeat(250),
  "/Users/qiushiyan/dev/" + "é".repeat(120) + "/\u{1F642}/" + "x_y ".repeat(30),
  "/" + "a".repeat(199),
  "/" + "a".repeat(200),
];
// one whose hash is negative before abs, to pin abs()
for (let i = 0; i < 2000; i++) { const s = "/p/" + "z".repeat(205) + i; if (Cbe(s) < 0) { inputs.push(s); break; } }
for (const s of inputs) console.log(JSON.stringify([s, U0(s)]));
