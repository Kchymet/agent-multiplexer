package core

// Item verbs: the control vocabulary of upward signals (internal/items). Every
// one names the session acting as Action.ID — for a restricted caller that is
// always the caller itself (access.allowSelfItem); the host may act as any
// session (the console, typically). The item an action moves is named by
// ItemIDField, never by Action.ID.
const (
	// ActionRaiseItem raises an item from Action.ID. Fields: ItemKindField,
	// ItemTitleField (required), ItemDetailField, ItemPRsField,
	// ItemLinksField, ItemSuggestField. The daemon routes it (a member to its
	// coordinator, anyone else to the console) and de-duplicates it against
	// the source's active items. Result.NewID is the item id and Result.Item
	// the stored item.
	ActionRaiseItem = "raise-item"
	// ActionItemState moves an item. Fields: ItemIDField, ItemStateField
	// (ack | dismiss | done | reopen, or the state name), ItemNoteField.
	ActionItemState = "item-state"
	// ActionEscalateItem hands an item a coordinator holds to the console.
	// Fields: ItemIDField, ItemNoteField.
	ActionEscalateItem = "escalate-item"
)

// QueryItems lists the items Action.ID may see (items.List): what it raised and
// what it receives — for a coordinator also its workgroup's, for the console
// all. Fields (all optional): ItemStatesField (comma-separated states, or
// "all"; default the active states), ItemBoxField, ItemLimitField, and
// ItemIDField to read one item.
const QueryItems = "items"

// Item fields.
const (
	ItemIDField      = "item"
	ItemKindField    = "kind"
	ItemTitleField   = "title"
	ItemDetailField  = "detail"
	ItemPRsField     = "prs"   // comma-separated
	ItemLinksField   = "links" // newline-separated
	ItemSuggestField = "suggest"
	ItemStateField   = "state"
	ItemNoteField    = "note"
	ItemStatesField  = "states"
	// ItemBoxField narrows a list: ItemBoxRaised (items the caller raised) or
	// ItemBoxInbox (items it currently receives).
	ItemBoxField   = "box"
	ItemLimitField = "limit"
)

// ItemBoxField values.
const (
	ItemBoxRaised = "raised"
	ItemBoxInbox  = "inbox"
)
