import type { MockData } from "./index";

// Fixtures for the setup fallback. The wizard no longer auto-detects or
// offers version pickers, but the mock service is global, so DetectInstalls
// / Profiles stay available for ?mock=1 flows that still query them.

export const data: MockData = {
  DetectInstalls: () => [
    {
      root: "C:\\Program Files (x86)\\World of Warcraft",
      flavor: "_retail_",
      addons_path:
        "C:\\Program Files (x86)\\World of Warcraft\\_retail_\\Interface\\AddOns",
      exe: "C:\\Program Files (x86)\\World of Warcraft\\_retail_\\Wow.exe",
      version: "11.0.2",
      profile_id: "retail",
      confidence: "high",
    },
    {
      root: "D:\\Games\\World of Warcraft",
      flavor: "_classic_",
      addons_path: "D:\\Games\\World of Warcraft\\_classic_\\Interface\\AddOns",
      exe: "D:\\Games\\World of Warcraft\\_classic_\\Wow.exe",
      version: "3.4.3",
      profile_id: "wrath",
      confidence: "high",
    },
    {
      root: "E:\\WoW Classic Era",
      flavor: "_classic_era_",
      addons_path: "E:\\WoW Classic Era\\_classic_era_\\Interface\\AddOns",
      exe: "E:\\WoW Classic Era\\_classic_era_\\Wow.exe",
      version: "1.15.4",
      profile_id: "era",
      confidence: "medium",
    },
  ],
  // The setup flow reloads the shell after SetInstall, so a static Install
  // value is enough for the mock.
  SetInstall: () => ({
    root: "C:\\Program Files (x86)\\World of Warcraft",
    flavor: "_retail_",
    addons_path:
      "C:\\Program Files (x86)\\World of Warcraft\\_retail_\\Interface\\AddOns",
    exe: "C:\\Program Files (x86)\\World of Warcraft\\_retail_\\Wow.exe",
    version: "11.0.2",
    profile_id: "retail",
    confidence: "high",
  }),
  SetProfile: () => {},
  Profiles: () => [
    { id: "retail", name: "Retail", family: "Dragonflight +", interface: 110200 },
    { id: "wrath", name: "Wrath Classic", family: "Wrath / Cataclysm Classic", interface: 30403 },
    { id: "era", name: "Classic Era", family: "Classic Era", interface: 11504 },
    { id: "tbc", name: "TBC Classic", family: "Burning Crusade Classic", interface: 20504 },
    { id: "vanilla", name: "Vanilla", family: "Original", interface: 11403 },
  ],
};
