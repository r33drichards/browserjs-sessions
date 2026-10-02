// A two-word name in the style of the petname packages, e.g. "brave-otter".
// Shown as the placeholder of the New session form and used when the field is
// left empty. The server makes up a name of its own for clients that send none.
const adjectives = [
  "able", "amused", "brave", "bright", "calm", "clever", "cosmic", "daring", "eager", "fair",
  "fancy", "gentle", "glad", "golden", "happy", "humble", "jolly", "keen", "kind", "lively",
  "lucky", "mellow", "merry", "mighty", "noble", "polite", "proud", "quick", "quiet", "rapid",
  "ready", "sharp", "smart", "snappy", "steady", "sunny", "swift", "tidy", "vivid", "witty",
]
const animals = [
  "badger", "bear", "beetle", "bison", "bobcat", "crane", "dingo", "dolphin", "eagle", "falcon",
  "ferret", "finch", "fox", "gecko", "heron", "ibex", "jackal", "koala", "lemur", "lynx",
  "marmot", "moose", "newt", "ocelot", "orca", "otter", "owl", "panda", "puffin", "quail",
  "raven", "robin", "seal", "shrew", "stork", "tapir", "tiger", "walrus", "wombat", "zebra",
]

function pick(words: string[]): string {
  return words[Math.floor(Math.random() * words.length)]
}

export function petname(): string {
  return `${pick(adjectives)}-${pick(animals)}`
}
