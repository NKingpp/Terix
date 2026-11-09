import packageJson from "../../package.json";

const currentYear = new Date().getFullYear();

export const APP_CONFIG = {
  name: "Terix",
  version: packageJson.version,
  copyright: `© ${currentYear}, Terix.`,
  meta: {
    title: "Terix — 金融分析执行平台",
    description: "多智能体金融分析 · 风控闸门 · 纸面执行",
  },
};
