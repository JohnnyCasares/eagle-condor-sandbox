import { test, expect } from "@playwright/test";
import { searchSpecies } from "./helpers.js";

test("Andean Condor — search suggests the species with its scientific name", async ({ page }) => {
  const option = await searchSpecies(page, "andean condor", "Andean Condor - Vultur gryphus");
  await expect(option).toHaveText(/Andean Condor\s*-\s*Vultur gryphus/);

  await option.click();
  await expect(page.getByText("Vultur gryphus").first()).toBeVisible();
});
