import { expect, test } from "@playwright/test";
import { searchSpecies } from "./helpers.js";

test("Bald Eagle — species page shows the scientific name", async ({ page }) => {
  const option = await searchSpecies(page, "bald eagle", "Bald Eagle - Haliaeetus");
  await option.click();

  const showcase = page.locator("#MediaFeed-showcase").getByText("Bald Eagle");
  await expect(showcase).toBeVisible();
  await showcase.click();

  await expect(page.getByText("This Is Definitely Not The Right Name").first()).toBeVisible();
});
