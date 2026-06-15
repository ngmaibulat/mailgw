import { Model, DataTypes } from "../../src/adapter.js";
import sequelize from "../config/sequelize.mjs";

export class Transaction extends Model {
    /**
     * Helper method for defining associations.
     * This method is not a part of Sequelize lifecycle.
     * The `models/index` file will call this method automatically.
     */
    static associate(models) {
        // define association here
    }
}

Transaction.init(
    {
        dt: DataTypes.DATE,
        uuid: DataTypes.STRING,
        action: DataTypes.STRING,
        encoding: DataTypes.STRING,
        sender: DataTypes.STRING,
        rcpt_list: DataTypes.STRING,
        rcpt_count_accept: DataTypes.INTEGER,
        rcpt_count_tempfail: DataTypes.INTEGER,
        rcpt_count_reject: DataTypes.INTEGER,
        delay_data_post: DataTypes.DOUBLE,
        data_bytes: DataTypes.INTEGER,
        mime_part_count: DataTypes.INTEGER,
    },
    {
        sequelize,
        modelName: "Transaction",
        freezeTableName: true,
    }
);

export default Transaction;
