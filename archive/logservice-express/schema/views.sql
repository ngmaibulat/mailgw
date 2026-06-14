
drop view if exists v_lookups;

create view v_lookups as
select
    h.*,
    dt,
    encoding,
    sender,
    rcpt_list,
    rcpt_count_accept,
    rcpt_count_tempfail,
    rcpt_count_reject,
    delay_data_post,
    data_bytes,
    mime_part_count,
    t.action as txn_action
from
    HashLookups h left join Transaction t on h.txn_uuid = t.uuid;


