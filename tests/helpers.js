import { expect } from "@playwright/test";

const HOME = "https://birdsoftheworld.org/bow/home";

/**
 * Types a species name into the home page search box and returns the matching
 * autocomplete option, already asserted visible. Shared by eagle.spec.js and
 * condor.spec.js so each can be its own pstad workflow (one spec = one
 * workflow in workflows.json) without duplicating the search steps.
 */
export async function searchSpecies(page, query, optionName) {
  await page.goto(HOME);

  const search = page.locator("#hero");
  await search.click();
  await search.fill(query);

  const option = page.getByRole("option", { name: optionName });
  await expect(option).toBeVisible();
  return option;
}
